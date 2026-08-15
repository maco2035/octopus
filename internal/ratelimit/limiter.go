// Package ratelimit implements a small in-memory, per-key fixed-window
// limiter — enough for PLAN.md Phase 8's requirement to rate limit
// /login, /api/slack/command, and the web UI's run-trigger endpoint on a
// single-process deployment. It's deliberately not distributed (no Redis,
// no shared state) since Octopus runs as one process against one SQLite
// database; a multi-process deployment would need a different approach,
// but that's not this project's shape.
package ratelimit

import (
	"net/http"
	"sync"
	"time"
)

type Limiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	hits map[string][]time.Time
}

// New returns a Limiter that allows at most limit calls to Allow per key
// within any window-long sliding period.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

// Allow reports whether key may proceed right now, recording the attempt
// either way isn't quite right — only a successful (allowed) call is
// recorded, so a client hammering a blocked endpoint doesn't get to "use
// up" slots it was never granted.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}

	l.hits[key] = append(kept, now)
	return true
}

// Middleware wraps next so a request is rejected with 429 Too Many
// Requests when key(r) has exceeded the limit. key is typically the
// client's remote address, but callers with a more specific identity
// (a logged-in username, a Slack team ID) should prefer that instead —
// it's harder to spoof and doesn't lump an entire NAT'd office together.
func (l *Limiter) Middleware(key func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(key(r)) {
			http.Error(w, "rate limit exceeded, try again shortly", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// RemoteAddrKey is the default key function: the client's remote address
// without its port, so multiple requests from the same client (whatever
// port their OS picked) share one bucket.
func RemoteAddrKey(r *http.Request) string {
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
