package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu        sync.Mutex
	rate      float64
	burst     float64
	tokens    float64
	updatedAt time.Time
	clock     func() time.Time
}

func New(ratePerSecond float64) *Limiter {
	return NewWithClock(ratePerSecond, ratePerSecond, time.Now)
}

func NewWithClock(ratePerSecond, burst float64, clock func() time.Time) *Limiter {
	return &Limiter{
		rate:      ratePerSecond,
		burst:     burst,
		tokens:    burst,
		updatedAt: clock(),
		clock:     clock,
	}
}

func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refillLocked()

	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// RetryAfter reports how long a caller should wait before the next token is
// available. It returns 0 if a token is available right now.
func (l *Limiter) RetryAfter() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refillLocked()

	if l.tokens >= 1 {
		return 0
	}
	deficit := 1 - l.tokens
	return time.Duration(deficit / l.rate * float64(time.Second))
}

// refillLocked mirrors the elapsed-time refill math in
// services/core/store/lua/token_bucket.lua, but reads the caller-supplied
// clock instead of Redis's server time. Callers must hold l.mu.
func (l *Limiter) refillLocked() {
	now := l.clock()
	elapsed := now.Sub(l.updatedAt)
	if elapsed <= 0 {
		return
	}
	l.tokens += elapsed.Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.updatedAt = now
}
