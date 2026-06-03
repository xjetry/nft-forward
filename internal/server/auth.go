package server

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nft-forward/internal/db"
)

const (
	sessionCookie = "nft_session"
	flashCookie   = "nft_flash"
	sessionTTL    = 12 * time.Hour
)

type ctxKey int

const userKey ctxKey = iota

func userFromCtx(ctx context.Context) *db.User {
	v, _ := ctx.Value(userKey).(*db.User)
	return v
}

func (s *Server) requireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := userFromCtx(r.Context())
			if u == nil || u.Role != role {
				http.Error(w, "权限不足", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		u, err := db.GetSessionUser(s.DB, c.Value)
		if err != nil || u == nil {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if u.Disabled {
			http.Error(w, "账号已被禁用", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Flash": flashFromCookie(w, r)})
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	u, err := db.GetUserByUsername(s.DB, username)
	if err != nil {
		setFlash(w, "用户名或密码错误")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(password)) != nil {
		setFlash(w, "用户名或密码错误")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if u.Disabled {
		setFlash(w, "账号已被禁用")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token, err := db.CreateSession(s.DB, u.ID, sessionTTL)
	if err != nil {
		log.Printf("CreateSession: %v", err)
		setFlash(w, "登录失败，请稍后再试")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	db.WriteAudit(s.DB, u.ID, "login", "", "")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = db.DeleteSession(s.DB, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func setFlash(w http.ResponseWriter, msg string) {
	// Cookie values must be ASCII-safe (RFC 6265); net/http silently drops
	// non-ASCII bytes, which would blank out non-ASCII (e.g. Chinese) flash
	// messages entirely. Percent-encode so UTF-8 survives the round trip.
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 30})
}

func flashFromCookie(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
	msg, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	return msg
}

func HashPassword(p string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func (s *Server) getChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	s.render(w, "change_password.html", map[string]any{
		"User":  u,
		"Flash": flashFromCookie(w, r),
	})
}

func (s *Server) postChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	oldPw := r.FormValue("old_password")
	newPw := r.FormValue("new_password")
	confirm := r.FormValue("confirm")
	if bcrypt.CompareHashAndPassword([]byte(u.PwHash), []byte(oldPw)) != nil {
		setFlash(w, "原密码不正确")
		http.Redirect(w, r, "/change-password", http.StatusSeeOther)
		return
	}
	if len(newPw) < 6 {
		setFlash(w, "新密码至少 6 位")
		http.Redirect(w, r, "/change-password", http.StatusSeeOther)
		return
	}
	if newPw != confirm {
		setFlash(w, "两次输入的新密码不一致")
		http.Redirect(w, r, "/change-password", http.StatusSeeOther)
		return
	}
	if newPw == oldPw {
		setFlash(w, "新密码与原密码相同")
		http.Redirect(w, r, "/change-password", http.StatusSeeOther)
		return
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		setFlash(w, "哈希失败")
		http.Redirect(w, r, "/change-password", http.StatusSeeOther)
		return
	}
	if _, err := s.DB.Exec(`UPDATE users SET pw_hash=? WHERE id=?`, hash, u.ID); err != nil {
		setFlash(w, err.Error())
		http.Redirect(w, r, "/change-password", http.StatusSeeOther)
		return
	}
	// invalidate other sessions, keep current one
	cur, _ := r.Cookie(sessionCookie)
	if cur != nil {
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, u.ID, cur.Value)
	}
	db.WriteAudit(s.DB, u.ID, "user.change_password", "", "")
	setFlash(w, "密码已更新")
	http.Redirect(w, r, "/change-password", http.StatusSeeOther)
}
