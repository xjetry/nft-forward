package server

import (
	"context"
	"net/http"
	"net/url"
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

func setFlash(w http.ResponseWriter, msg string) {
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
