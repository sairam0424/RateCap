package negativecache

import (
	"sync"
	"time"
)

// sweepThreshold bounds unbounded memory growth from a flood of distinct
// denied keys: an opportunistic sweep on MarkDenied, rather than a
// background goroutine, keeps this package dependency-free of any
// lifecycle/shutdown concern.
const sweepThreshold = 10000

type Cache struct {
	mu     sync.Mutex
	denied map[string]time.Time
	clock  func() time.Time
}

func New() *Cache {
	return NewWithClock(time.Now)
}

func NewWithClock(clock func() time.Time) *Cache {
	return &Cache{denied: make(map[string]time.Time), clock: clock}
}

// MarkDenied records that key was rejected and should short-circuit future
// checks until retryAfter elapses — exactly as long as the real decision
// already told the caller to wait, not a heuristic guess.
func (c *Cache) MarkDenied(key string, retryAfter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denied[key] = c.clock().Add(retryAfter)
	if len(c.denied) > sweepThreshold {
		c.sweepLocked()
	}
}

func (c *Cache) IsDenied(key string) (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.denied[key]
	if !ok {
		return false, 0
	}
	now := c.clock()
	if now.After(until) {
		delete(c.denied, key)
		return false, 0
	}
	return true, until.Sub(now)
}

func (c *Cache) sweepLocked() {
	now := c.clock()
	for k, until := range c.denied {
		if now.After(until) {
			delete(c.denied, k)
		}
	}
}
