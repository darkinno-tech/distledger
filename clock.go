package distledger

import (
	"sync"
	"time"
)

// Clock abstracts the current time.
//
// The library **must not** call time.Now() directly: every timestamp comes from an
// injected Clock. That lets freeze windows, settlement instants, and binding
// deadlines be advanced instantly in tests, with no sleeps and no unacceptable
// "the test took 7 days" cases (see ADR-007).
type Clock interface {
	// Now returns the current time. Implementations must return UTC.
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// SystemClock returns a Clock backed by the system time. This is the default
// implementation.
func SystemClock() Clock { return systemClock{} }

// ManualClock is a Clock that can be advanced by hand, for tests and simulations.
//
// It is safe for concurrent use. The zero value is not usable; construct one with
// NewManualClock.
type ManualClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewManualClock returns a ManualClock stopped at t (normalized to UTC).
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t.UTC()}
}

// Now returns the currently set time.
func (c *ManualClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance moves the clock forward by d. A negative d moves it backward.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set sets the clock to t.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}
