package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Login brute-force guard. bcrypt already makes offline guessing slow, but the
// online endpoint has no cost to the attacker without this — a small in-memory
// limiter (no external store, matching the project's thin-dependency stance) is
// enough. Keying by (client IP + username) blocks cracking a single account from
// a source while avoiding collateral lockout when many users share one proxy IP.
const (
	loginMaxFailures = 8
	loginWindow      = 10 * time.Minute
	loginLockout     = 10 * time.Minute
)

type loginAttempt struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

type loginLimiter struct {
	mu   sync.Mutex
	byID map[string]*loginAttempt
	now  func() time.Time // injectable for tests
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{byID: map[string]*loginAttempt{}, now: time.Now}
}

// allowed reports whether key may attempt a login now (i.e. not locked out).
func (l *loginLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.byID[key]
	if a == nil {
		return true
	}
	return !l.now().Before(a.lockedUntil)
}

// recordFailure counts a failed attempt and locks the key once it crosses the
// threshold inside the rolling window.
func (l *loginLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.byID[key]
	if a == nil || now.Sub(a.windowStart) > loginWindow {
		a = &loginAttempt{windowStart: now}
		l.byID[key] = a
	}
	a.failures++
	if a.failures >= loginMaxFailures {
		a.lockedUntil = now.Add(loginLockout)
	}
	l.pruneLocked(now)
}

// recordSuccess clears any accumulated failures for the key.
func (l *loginLimiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byID, key)
}

// pruneLocked drops entries whose window has elapsed and whose lockout has
// expired, so the map can't grow unbounded. Caller holds l.mu.
func (l *loginLimiter) pruneLocked(now time.Time) {
	for k, a := range l.byID {
		if now.Sub(a.windowStart) > loginWindow && now.After(a.lockedUntil) {
			delete(l.byID, k)
		}
	}
}

// clientIP extracts the remote IP from a request, tolerating a missing port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
