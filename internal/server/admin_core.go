package server

import (
	"net/http"
	"strconv"
	"strings"

	"nft-forward/internal/db"
)

// adminError carries a v1-mappable failure out of a shared admin mutation core.
// The cores are called by BOTH the SPA (/api) and public (/api/v1) handlers, so
// they must not write to a ResponseWriter themselves — each surface renders the
// error in its own envelope.
type adminError struct {
	status int
	code   string
	msg    string
}

func (e *adminError) Error() string { return e.msg }

// writeAdminErrV1 renders an adminError in the /api/v1 envelope.
func writeAdminErrV1(w http.ResponseWriter, e *adminError) {
	v1Err(w, e.status, e.code, e.msg)
}

// provisionParams are the normalized inputs to provisionUser. Callers decode
// their own wire shape (SPA takes an expires_at date string; v1 takes unix) and
// hand the core already-normalized values.
type provisionParams struct {
	Username          string
	Password          string // caller ensures non-empty (generate a random one for API-only users)
	Role              string
	MaxForwards       int
	TrafficQuotaBytes int64
	ExpiresAtUnix     int64 // 0 = no expiry
	LandingSubURL     string
	AdminNote         string
}

// provisionUser creates a user with billing/quota defaults and audits it. A
// duplicate username is a conflict (not a 500) so the v1 surface can report it
// cleanly. Shared by apiCreateUser (SPA) and v1AdminCreateUser.
func (s *Server) provisionUser(adminID int64, p provisionParams) (*db.User, *adminError) {
	username := strings.TrimSpace(p.Username)
	if username == "" || p.Password == "" {
		return nil, &adminError{http.StatusBadRequest, codeValidation, "用户名和密码不能为空"}
	}
	role := p.Role
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		return nil, &adminError{http.StatusBadRequest, codeValidation, "角色须为 admin 或 user"}
	}
	if _, err := db.GetUserByUsername(s.DB, username); err == nil {
		return nil, &adminError{http.StatusConflict, codeConflict, "用户名已存在"}
	}
	hash, err := HashPassword(p.Password)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	id, err := db.CreateUser(s.DB, username, hash, role)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	maxFwd := p.MaxForwards
	if maxFwd <= 0 {
		maxFwd = 100
	}
	if _, err := s.DB.Exec(`UPDATE users SET max_forwards=?, traffic_quota_bytes=? WHERE id=?`,
		maxFwd, p.TrafficQuotaBytes, id); err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	if p.ExpiresAtUnix != 0 {
		s.DB.Exec(`UPDATE users SET expires_at=? WHERE id=?`, p.ExpiresAtUnix, id)
	}
	if p.LandingSubURL != "" || p.AdminNote != "" {
		s.DB.Exec(`UPDATE users SET landing_sub_url=?, admin_note=? WHERE id=?`,
			strings.TrimSpace(p.LandingSubURL), strings.TrimSpace(p.AdminNote), id)
	}
	db.WriteAudit(s.DB, adminID, "user.create", strconv.FormatInt(id, 10), username)
	created, err := db.GetUserByID(s.DB, id)
	if err != nil {
		return nil, &adminError{http.StatusInternalServerError, codeInternal, err.Error()}
	}
	return created, nil
}
