package bus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

// MemoryOptions parameterises the in-process driver.
type MemoryOptions struct {
	// Synchronous delivers events on the publishing goroutine using a
	// trampoline queue, which gives a single deterministic interleaving. This
	// is what the backtester and the demo runner use: with it, the same input
	// always produces the same output, byte for byte.
	Synchronous bool
	// Buffer is the per-subscription queue depth in asynchronous mode.
	Buffer int
	// Metrics is optional.
	Metrics *obs.Metrics
	// OnError is called for handler errors after retries are exhausted. In
	// asynchronous mode a nil OnError drops the event and increments a counter.
	OnError func(topic, group string, e Envelope, err error)
	// MaxRetries per event before the error path is taken.
	MaxRetries int
}

type memSub struct {
	topic   string
	group   string
	handler Handler
	queue   chan Envelope
	offset  int64
}

// MemoryBus is an in-process Bus preserving publication order per topic.
//
// Ordering guarantee: for a given topic and consumer group, events are
// delivered in publication order. That is strictly stronger than Kafka's
// per-partition guarantee, so code written against MemoryBus is safe on Kafka
// provided it does not *rely* on cross-key ordering — which the architecture
// test asserts by also exercising the shuffled-partition case.
type MemoryBus struct {
	opts MemoryOptions

	mu     sync.RWMutex
	subs   map[string][]*memSub // topic -> subscriptions
	closed bool

	// trampoline state for synchronous mode
	dispatching bool
	pending     []pendingEvent

	wg      sync.WaitGroup
	running bool
	runOnce sync.Once
	stopCh  chan struct{}
}

type pendingEvent struct {
	topic string
	env   Envelope
}

// NewMemoryBus returns an in-process bus.
func NewMemoryBus(opts MemoryOptions) *MemoryBus {
	if opts.Buffer <= 0 {
		opts.Buffer = 4096
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	return &MemoryBus{
		opts:   opts,
		subs:   map[string][]*memSub{},
		stopCh: make(chan struct{}),
	}
}

// Subscribe registers a handler. Subscriptions are kept sorted by group name so
// that delivery order across groups is deterministic.
func (b *MemoryBus) Subscribe(topic, group string, h Handler) error {
	if h == nil {
		return errors.New("bus: nil handler")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	s := &memSub{topic: topic, group: group, handler: h}
	if !b.opts.Synchronous {
		s.queue = make(chan Envelope, b.opts.Buffer)
	}
	b.subs[topic] = append(b.subs[topic], s)
	sort.SliceStable(b.subs[topic], func(i, j int) bool {
		return b.subs[topic][i].group < b.subs[topic][j].group
	})
	if b.running && !b.opts.Synchronous {
		b.startSub(s)
	}
	return nil
}

// Publish delivers one event.
func (b *MemoryBus) Publish(ctx context.Context, topic string, e Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if b.opts.Metrics != nil {
		b.opts.Metrics.BusPublished.Inc(topic)
	}
	if b.opts.Synchronous {
		return b.publishSync(ctx, topic, e)
	}
	return b.publishAsync(ctx, topic, e)
}

// PublishBatch publishes events in order.
func (b *MemoryBus) PublishBatch(ctx context.Context, topic string, es []Envelope) error {
	for _, e := range es {
		if err := b.Publish(ctx, topic, e); err != nil {
			return err
		}
	}
	return nil
}

// publishSync uses a trampoline so that handlers publishing further events do
// not recurse: the causal chain is processed breadth-first in a single
// deterministic order.
func (b *MemoryBus) publishSync(ctx context.Context, topic string, e Envelope) error {
	b.mu.Lock()
	if b.dispatching {
		b.pending = append(b.pending, pendingEvent{topic, e})
		b.mu.Unlock()
		return nil
	}
	b.dispatching = true
	b.mu.Unlock()

	queue := []pendingEvent{{topic, e}}
	var firstErr error
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		b.mu.RLock()
		subs := append([]*memSub(nil), b.subs[item.topic]...)
		b.mu.RUnlock()

		for _, s := range subs {
			env := item.env
			env.Offset = s.offset
			s.offset++
			if err := b.deliver(ctx, s, env); err != nil && firstErr == nil {
				firstErr = err
			}
		}

		b.mu.Lock()
		queue = append(queue, b.pending...)
		b.pending = nil
		b.mu.Unlock()
	}

	b.mu.Lock()
	b.dispatching = false
	b.mu.Unlock()
	return firstErr
}

func (b *MemoryBus) publishAsync(ctx context.Context, topic string, e Envelope) error {
	b.mu.RLock()
	subs := append([]*memSub(nil), b.subs[topic]...)
	b.mu.RUnlock()
	for _, s := range subs {
		env := e
		select {
		case s.queue <- env:
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Bounded queue full: this is backpressure, and dropping silently
			// would be a correctness bug. Block with cancellation instead.
			select {
			case s.queue <- env:
			case <-ctx.Done():
				return ctx.Err()
			case <-b.stopCh:
				return ErrClosed
			}
		}
	}
	return nil
}

func (b *MemoryBus) deliver(ctx context.Context, s *memSub, e Envelope) error {
	hctx := ContextFor(ctx, e)
	var err error
	for attempt := 0; attempt <= b.opts.MaxRetries; attempt++ {
		err = s.handler(hctx, e)
		if err == nil {
			if b.opts.Metrics != nil {
				b.opts.Metrics.BusConsumed.Inc(s.topic, s.group)
			}
			return nil
		}
	}
	if b.opts.Metrics != nil {
		b.opts.Metrics.BusErrors.Inc(s.topic, "handle")
	}
	if b.opts.OnError != nil {
		b.opts.OnError(s.topic, s.group, e, err)
		return nil
	}
	return fmt.Errorf("bus: topic=%s group=%s event=%s: %w", s.topic, s.group, e.EventID, err)
}

func (b *MemoryBus) startSub(s *memSub) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case <-b.stopCh:
				return
			case e, ok := <-s.queue:
				if !ok {
					return
				}
				e.Offset = s.offset
				s.offset++
				_ = b.deliver(context.Background(), s, e)
			}
		}
	}()
}

// Run starts asynchronous delivery and blocks until ctx is done. In
// synchronous mode it simply waits, because delivery happens inline.
func (b *MemoryBus) Run(ctx context.Context) error {
	b.runOnce.Do(func() {
		b.mu.Lock()
		b.running = true
		subs := make([]*memSub, 0)
		for _, list := range b.subs {
			subs = append(subs, list...)
		}
		b.mu.Unlock()
		if !b.opts.Synchronous {
			for _, s := range subs {
				b.startSub(s)
			}
		}
	})
	select {
	case <-ctx.Done():
		return nil
	case <-b.stopCh:
		return nil
	}
}

// Drain blocks until every queued event has been handled. It is only meaningful
// in asynchronous mode; in synchronous mode delivery is already complete when
// Publish returns.
func (b *MemoryBus) Drain(ctx context.Context) error {
	if b.opts.Synchronous {
		return nil
	}
	for {
		b.mu.RLock()
		total := 0
		for _, list := range b.subs {
			for _, s := range list {
				total += len(s.queue)
			}
		}
		b.mu.RUnlock()
		if total == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// Close stops delivery and releases resources. It is idempotent.
func (b *MemoryBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	close(b.stopCh)
	b.wg.Wait()
	return nil
}
