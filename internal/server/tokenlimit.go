package server

import (
	"sync"
	"time"
)

// tokenLimiter rate-limits /api/v1 requests per token and throttles the
// last_used_at bookkeeping write so a polling agent can't turn every read into a
// database UPDATE. In-memory only, matching the project's thin-dependency stance
// (see loginLimiter); losing the state on restart only resets counters, which is
// harmless.
const (
	tokenRateWindow  = time.Second
	tokenRateBurst   = 20 // requests per window per token
	lastUsedInterval = time.Minute
	tokenIdleExpiry  = 10 * time.Minute
)

type tokenState struct {
	windowStart time.Time
	count       int
	lastTouch   time.Time
}

type tokenLimiter struct {
	mu   sync.Mutex
	byID map[int64]*tokenState
	now  func() time.Time // injectable for tests
}

func newTokenLimiter() *tokenLimiter {
	return &tokenLimiter{byID: map[int64]*tokenState{}, now: time.Now}
}

func (l *tokenLimiter) stateLocked(id int64) *tokenState {
	st := l.byID[id]
	if st == nil {
		st = &tokenState{}
		l.byID[id] = st
	}
	return st
}

// allow reports whether tokenID may make another request now, advancing its
// per-token fixed-window counter and pruning idle tokens.
func (l *tokenLimiter) allow(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.stateLocked(id)
	if now.Sub(st.windowStart) >= tokenRateWindow {
		st.windowStart = now
		st.count = 0
	}
	st.count++
	l.pruneLocked(now)
	return st.count <= tokenRateBurst
}

// shouldTouch reports whether last_used_at is stale enough (older than
// lastUsedInterval, or never written this process) to be worth a DB write,
// stamping the in-memory marker when it says yes.
func (l *tokenLimiter) shouldTouch(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.stateLocked(id)
	if st.lastTouch.IsZero() || now.Sub(st.lastTouch) >= lastUsedInterval {
		st.lastTouch = now
		return true
	}
	return false
}

// pruneLocked drops tokens idle past tokenIdleExpiry so the map can't grow
// unbounded. Caller holds l.mu.
func (l *tokenLimiter) pruneLocked(now time.Time) {
	for k, st := range l.byID {
		if now.Sub(st.windowStart) > tokenIdleExpiry && now.Sub(st.lastTouch) > tokenIdleExpiry {
			delete(l.byID, k)
		}
	}
}
