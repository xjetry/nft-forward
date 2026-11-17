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
