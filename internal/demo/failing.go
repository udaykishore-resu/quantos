package demo

import (
	"context"
	"errors"

	"github.com/udaykishoreresu/quantos/internal/bus"
)

// errBusDown is what a broker outage looks like to a producer.
var errBusDown = errors.New("bus: broker unavailable (simulated outage)")

// failingBus wraps a bus and fails every publish while `fail` is set.
//
// It exists so the failure scenarios exercise the *real* error paths rather
// than a narrative about them: with it in place, a publish returns an error,
// the producer's metric increments, and the caller must decide what to do —
// which is exactly the behaviour under a genuine broker outage.
type failingBus struct {
	inner bus.Bus
	fail  bool
	// FailConsume additionally makes subscription registration fail, which
	// models a broker that is unreachable at start-up rather than mid-run.
	failConsume bool
}

// Publish implements bus.Publisher.
func (f *failingBus) Publish(ctx context.Context, topic string, e bus.Envelope) error {
	if f.fail {
		return errBusDown
	}
	return f.inner.Publish(ctx, topic, e)
}

// PublishBatch implements bus.Publisher.
func (f *failingBus) PublishBatch(ctx context.Context, topic string, es []bus.Envelope) error {
	if f.fail {
		return errBusDown
	}
	return f.inner.PublishBatch(ctx, topic, es)
}

// Subscribe implements bus.Subscriber.
func (f *failingBus) Subscribe(topic, group string, h bus.Handler) error {
	if f.failConsume {
		return errBusDown
	}
	return f.inner.Subscribe(topic, group, h)
}

// Run implements bus.Subscriber.
func (f *failingBus) Run(ctx context.Context) error { return f.inner.Run(ctx) }

// Close implements bus.Publisher.
func (f *failingBus) Close() error { return nil }
