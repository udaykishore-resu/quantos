// Package obs provides the cross-cutting concerns every QuantOS component
// needs: a controllable clock, structured logging, Prometheus-compatible
// metrics and OpenTelemetry-compatible tracing.
//
// The metrics and tracing implementations are written against the wire formats
// (Prometheus text exposition, OTLP/HTTP JSON) rather than pulling in the
// upstream SDKs. That keeps the dependency surface of a financial platform
// small and auditable, and both formats are stable, documented contracts.
package obs

import (
	"sync"
	"time"
)

// Clock abstracts time. Production code must never call time.Now() directly:
// backtests and tests need a controllable clock, and determinism is a stated
// requirement (ADR-007).
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
	NewTimer(d time.Duration) *time.Timer
	Sleep(d time.Duration)
}

// SystemClock is the wall-clock implementation used in services.
type SystemClock struct{}

func (SystemClock) Now() time.Time                       { return time.Now().UTC() }
func (SystemClock) Since(t time.Time) time.Duration      { return time.Since(t) }
func (SystemClock) NewTimer(d time.Duration) *time.Timer { return time.NewTimer(d) }
func (SystemClock) Sleep(d time.Duration)                { time.Sleep(d) }

// SimClock is a manually advanced clock. It is safe for concurrent use and is
// what makes backtests and demo scenarios reproducible.
type SimClock struct {
	mu  sync.RWMutex
	now time.Time
	// timers are fired when the simulated clock passes their deadline.
	timers []*simTimer
}

type simTimer struct {
	deadline time.Time
	timer    *time.Timer
	fired    bool
}

// NewSimClock returns a clock pinned to start.
func NewSimClock(start time.Time) *SimClock {
	return &SimClock{now: start.UTC()}
}

func (c *SimClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *SimClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// NewTimer returns a timer that fires when the simulated clock advances past d.
func (c *SimClock) NewTimer(d time.Duration) *time.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := time.NewTimer(time.Duration(1<<62 - 1))
	c.timers = append(c.timers, &simTimer{deadline: c.now.Add(d), timer: t})
	return t
}

// Sleep is a no-op advance on a simulated clock: the event loop, not the
// sleeper, controls time.
func (c *SimClock) Sleep(d time.Duration) { c.Advance(d) }

// Advance moves the clock forward and fires any due timers.
func (c *SimClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []*simTimer
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.fired && !t.deadline.After(now) {
			t.fired = true
			due = append(due, t)
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
	c.mu.Unlock()
	for _, t := range due {
		t.timer.Reset(0)
	}
	return now
}

// Set moves the clock to an absolute time. It must not move backwards.
func (c *SimClock) Set(t time.Time) {
	c.mu.Lock()
	if t.After(c.now) {
		c.mu.Unlock()
		c.Advance(t.Sub(c.Now()))
		return
	}
	c.now = t.UTC()
	c.mu.Unlock()
}
