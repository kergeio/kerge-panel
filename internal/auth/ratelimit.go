package auth

import (
	"sync"
	"time"
)

// Limiter applies exponential backoff to repeated failures per key (for
// example a client IP or a username). State is kept in memory only.
type Limiter struct {
	free    int           // failures allowed before backoff starts
	base    time.Duration // first backoff
	max     time.Duration // longest backoff
	idleTTL time.Duration // entries unused this long are dropped
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*limitEntry
	lastGC  time.Time
}

type limitEntry struct {
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

// NewLimiter returns the login limiter: 5 free failures, then 1s doubling
// up to 15 minutes.
func NewLimiter() *Limiter {
	return &Limiter{
		free:    5,
		base:    time.Second,
		max:     15 * time.Minute,
		idleTTL: 24 * time.Hour,
		now:     time.Now,
		entries: make(map[string]*limitEntry),
	}
}

// Blocked returns how long key must still wait, or 0 if it may try now.
func (l *Limiter) Blocked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return 0
	}
	if d := e.blockedUntil.Sub(l.now()); d > 0 {
		return d
	}
	return 0
}

// Fail records a failure for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)
	e := l.entries[key]
	if e == nil {
		e = &limitEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.lastSeen = now
	if over := e.failures - l.free; over > 0 {
		d := l.max
		if over <= 30 { // avoid shift overflow; 2^30 s is far above max
			if b := l.base << (over - 1); b < l.max {
				d = b
			}
		}
		e.blockedUntil = now.Add(d)
	}
}

// Reset clears the state of key, e.g. after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// gc drops idle entries at most once per minute. The caller holds mu.
func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	for k, e := range l.entries {
		if now.Sub(e.lastSeen) > l.idleTTL && !now.Before(e.blockedUntil) {
			delete(l.entries, k)
		}
	}
}
