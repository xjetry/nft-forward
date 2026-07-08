package server

import (
	"net/http"
	"strings"

	"nft-forward/internal/db"
)

// v1RequireUser writes a 404 and returns false when no user with id exists, so
// admin write handlers reject bad ids before mutating.
func (s *Server) v1RequireUser(w http.ResponseWriter, id int64) bool {
	if _, err := db.GetUserByID(s.DB, id); err != nil {
		v1Err(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return false
	}
	return true
}

// v1AdminCreateUser provisions a user and, unless issue_token is explicitly
// false, mints their token in the same call — the closed-loop onboarding an
// agent uses to hand off a ready-to-use API consumer. The plaintext token is
// returned once.
func (s *Server) v1AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	admin := userFromCtx(r.Context())
	var body struct {
		Username          string `json:"username"`
		Password          string `json:"password"`
		Role              string `json:"role"`
		MaxForwards       int    `json:"max_forwards"`
		TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
		ExpiresAt         int64  `json:"expires_at"`
		IssueToken        *bool  `json:"issue_token"`
		TokenScope        string `json:"token_scope"`
	}
	if err := decodeJSON(r, &body); err != nil {
		v1Err(w, http.StatusBadRequest, codeValidation, "请求格式错误")
		return
	}
	password := body.Password
	if strings.TrimSpace(password) == "" {
		// API-only consumers authenticate by token, not password. Give them a
		// random, unguessable one rather than an empty/known secret.
		password = db.RandToken(16)
	}
	user, aerr := s.provisionUser(admin.ID, provisionParams{
		Username: body.Username, Password: password, Role: body.Role,
		MaxForwards: body.MaxForwards, TrafficQuotaBytes: body.TrafficQuotaBytes,
		ExpiresAtUnix: body.ExpiresAt,
	})
	if aerr != nil {
		writeAdminErrV1(w, aerr)
		return
	}
	out := map[string]any{"user": toV1User(user)}
	issue := body.IssueToken == nil || *body.IssueToken // default true
	if issue {
		scope := db.NormalizeTokenScope(body.TokenScope)
		tok, _, err := db.IssueUserToken(s.DB, user.ID, scope)
		if err != nil {
			v1Err(w, http.StatusInternalServerError, codeInternal, "建 token 失败")
			return
		}
		out["token"] = tok
		out["token_scope"] = scope
	}
	v1OK(w, out)
}
