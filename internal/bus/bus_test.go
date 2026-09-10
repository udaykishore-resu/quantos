package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

var base = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func env(t *testing.T, b *Builder, typ, key string, payload any) Envelope {
	t.Helper()
	e, err := b.New(context.Background(), typ, key, base, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEnvelopeValidation(t *testing.T) {
	b := NewBuilder("test", obs.NewSimClock(base))
	good := env(t, b, TopicBars, "AAPL", map[string]int{"x": 1})
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed envelope was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Envelope){
		"empty event id": func(e *Envelope) { e.EventID = "" },
		"empty type":     func(e *Envelope) { e.Type = "" },
		"zero occurred":  func(e *Envelope) { e.OccurredAt = time.Time{} },
		"empty payload":  func(e *Envelope) { e.Payload = nil },
		"zero schema":    func(e *Envelope) { e.SchemaVersion = 0 },
	} {
		bad := good
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// TestEventIDsAreTimeOrdered checks the UUIDv7 property the WAL and the dedup
// index both rely on.
func TestEventIDsAreTimeOrdered(t *testing.T) {
	prev := ""
	for i := 0; i < 500; i++ {
		id := NewEventID(base.Add(time.Duration(i) * time.Millisecond))
		if id <= prev {
			t.Fatalf("event ids are not time-ordered: %q followed %q", id, prev)
		}
		prev = id
	}
}

func TestEventIDsAreUniqueWithinAMillisecond(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		id := NewEventID(base)
		if seen[id] {
			t.Fatalf("duplicate event id after %d draws", i)
		}
		seen[id] = true
	}
}

func TestCausationChainIsPreserved(t *testing.T) {
	b := NewBuilder("test", obs.NewSimClock(base))
	ctx := obs.WithCorrelationID(context.Background(), "corr-1")
	parent, err := b.New(ctx, TopicBars, "AAPL", base, map[string]int{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	child, err := b.Caused(ctx, parent, TopicPrediction, "AAPL", base, map[string]int{"y": 2})
	if err != nil {
		t.Fatal(err)
	}
	if child.CausationID != parent.EventID {
		t.Fatal("the child does not point at its cause; provenance would be unwalkable")
	}
	if child.CorrelationID != parent.CorrelationID {
		t.Fatal("the correlation id did not survive the causal hop")
	}
}

// --- memory bus -------------------------------------------------------------

func TestMemoryBusSynchronousOrdering(t *testing.T) {
	b := NewMemoryBus(MemoryOptions{Synchronous: true})
	defer b.Close()

	var got []int
	if err := b.Subscribe(TopicBars, "g1", func(_ context.Context, e Envelope) error {
		var v struct{ N int }
		if err := e.Decode(&v); err != nil {
			return err
		}
		got = append(got, v.N)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	builder := NewBuilder("test", obs.NewSimClock(base))
	for i := 0; i < 100; i++ {
		if err := b.Publish(context.Background(), TopicBars, env(t, builder, TopicBars, "AAPL", struct{ N int }{i})); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 100 {
		t.Fatalf("delivered %d of 100 events", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order at position %d: %d", i, v)
		}
	}
}

// TestMemoryBusTrampolineAvoidsRecursion checks that a handler which publishes
// further events is processed breadth-first rather than recursively, which is
// what keeps a long causal chain from exhausting the stack.
func TestMemoryBusTrampolineAvoidsRecursion(t *testing.T) {
	b := NewMemoryBus(MemoryOptions{Synchronous: true})
	defer b.Close()
	builder := NewBuilder("test", obs.NewSimClock(base))

	depth := 0
	maxDepth := 0
	if err := b.Subscribe(TopicBars, "g", func(ctx context.Context, e Envelope) error {
		depth++
		if depth > maxDepth {
			maxDepth = depth
		}
		var v struct{ N int }
		_ = e.Decode(&v)
		if v.N < 500 {
			_ = b.Publish(ctx, TopicBars, env(t, builder, TopicBars, "AAPL", struct{ N int }{v.N + 1}))
		}
		depth--
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(context.Background(), TopicBars, env(t, builder, TopicBars, "AAPL", struct{ N int }{0})); err != nil {
		t.Fatal(err)
	}
	if maxDepth > 1 {
		t.Fatalf("handlers nested %d deep; the trampoline is not working", maxDepth)
	}
}

func TestMemoryBusAsyncDelivery(t *testing.T) {
	b := NewMemoryBus(MemoryOptions{Buffer: 1024})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	count := 0
	if err := b.Subscribe(TopicQuotes, "g", func(_ context.Context, _ Envelope) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = b.Run(ctx) }()

	builder := NewBuilder("test", obs.NewSimClock(base))
	for i := 0; i < 200; i++ {
		if err := b.Publish(ctx, TopicQuotes, env(t, builder, TopicQuotes, "AAPL", struct{ N int }{i})); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := count
		mu.Unlock()
		if n == 200 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("only %d of 200 events were delivered", count)
}

// --- idempotency ------------------------------------------------------------

func TestIdempotentHandlerSuppressesRedelivery(t *testing.T) {
	d := NewMemoryDeduper(obs.NewSimClock(base), 1000)
	calls := 0
	h := Idempotent(d, IdempotentOptions{Group: "g"}, func(context.Context, Envelope) error {
		calls++
		return nil
	})
	builder := NewBuilder("test", obs.NewSimClock(base))
	e := env(t, builder, TopicSignal, "sig", map[string]int{"x": 1})

	for i := 0; i < 10; i++ {
		if err := h(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("the handler ran %d times for one event id", calls)
	}
}

// TestIdempotentHandlerReleasesOnFailure is the important half: a transient
// failure must not permanently swallow an event.
func TestIdempotentHandlerReleasesOnFailure(t *testing.T) {
	d := NewMemoryDeduper(obs.NewSimClock(base), 1000)
	calls := 0
	h := Idempotent(d, IdempotentOptions{Group: "g"}, func(context.Context, Envelope) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("transient")
		}
		return nil
	})
	builder := NewBuilder("test", obs.NewSimClock(base))
	e := env(t, builder, TopicSignal, "sig", map[string]int{"x": 1})

	if err := h(context.Background(), e); err == nil {
		t.Fatal("the first attempt should have returned the handler's error")
	}
	if err := h(context.Background(), e); err != nil {
		t.Fatalf("the retry was suppressed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("handler ran %d times; a failed claim was not released", calls)
	}
}

func TestDifferentGroupsEachSeeTheEvent(t *testing.T) {
	d := NewMemoryDeduper(obs.NewSimClock(base), 1000)
	builder := NewBuilder("test", obs.NewSimClock(base))
	e := env(t, builder, TopicSignal, "sig", map[string]int{"x": 1})

	a, bcount := 0, 0
	ha := Idempotent(d, IdempotentOptions{Group: "alpha"}, func(context.Context, Envelope) error { a++; return nil })
	hb := Idempotent(d, IdempotentOptions{Group: "beta"}, func(context.Context, Envelope) error { bcount++; return nil })
	_ = ha(context.Background(), e)
	_ = hb(context.Background(), e)
	if a != 1 || bcount != 1 {
		t.Fatalf("consumer groups are not independent: alpha=%d beta=%d", a, bcount)
	}
}

func TestEffectKeyIsStableAndDistinct(t *testing.T) {
	a := EffectKey("alert", "AAPL", "VWAP_CROSS", "2026-03-10T15:00:00Z")
	b := EffectKey("alert", "AAPL", "VWAP_CROSS", "2026-03-10T15:00:00Z")
	c := EffectKey("alert", "AAPL", "VWAP_CROSS", "2026-03-10T15:01:00Z")
	if a != b {
		t.Fatal("the same effect produced two different keys")
	}
	if a == c {
		t.Fatal("different time buckets produced the same key")
	}
}

func TestMaxAgeFilterConvertsLagIntoSilence(t *testing.T) {
	clock := obs.NewSimClock(base)
	dropped := 0
	handled := 0
	h := MaxAgeFilter(clock, time.Minute, func(Envelope) { dropped++ },
		func(context.Context, Envelope) error { handled++; return nil })

	builder := NewBuilder("test", clock)
	fresh := env(t, builder, TopicBars, "AAPL", map[string]int{"x": 1})
	stale := fresh
	stale.OccurredAt = base.Add(-10 * time.Minute)

	_ = h(context.Background(), fresh)
	_ = h(context.Background(), stale)

	if handled != 1 || dropped != 1 {
		t.Fatalf("handled=%d dropped=%d; a lagging consumer must go quiet, not act on old data", handled, dropped)
	}
}

// --- WAL bus ----------------------------------------------------------------

func TestWALDurabilityAndReplay(t *testing.T) {
	dir := t.TempDir()
	builder := NewBuilder("test", obs.NewSimClock(base))

	// Write, then close: the events must survive the process.
	b1, err := NewWALBus(WALOptions{Dir: dir, SyncEveryWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := b1.Publish(context.Background(), TopicBars, env(t, builder, TopicBars, "AAPL", struct{ N int }{i})); err != nil {
			t.Fatal(err)
		}
	}
	if d := b1.Depth(TopicBars); d != 50 {
		t.Fatalf("depth %d after fifty writes", d)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and consume from the beginning.
	b2, err := NewWALBus(WALOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if d := b2.Depth(TopicBars); d != 50 {
		t.Fatalf("after reopening, depth is %d; the log did not survive", d)
	}

	// The handler runs on the consumer goroutine; the slice is read from
	// this one, so access is serialised by a mutex.
	var (
		mu  sync.Mutex
		got []int
	)
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}
	if err := b2.Subscribe(TopicBars, "replay", func(_ context.Context, e Envelope) error {
		var v struct{ N int }
		if err := e.Decode(&v); err != nil {
			return err
		}
		mu.Lock()
		got = append(got, v.N)
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = b2.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && count() < 50 {
		time.Sleep(5 * time.Millisecond)
	}
	if count() != 50 {
		t.Fatalf("replayed %d of 50 events", count())
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range got {
		if v != i {
			t.Fatalf("replay order broken at %d: %d", i, v)
		}
	}
}

// TestWALResumesFromCommittedOffset checks the property a restart depends on:
// a consumer group picks up where it left off rather than replaying everything.
func TestWALResumesFromCommittedOffset(t *testing.T) {
	dir := t.TempDir()
	builder := NewBuilder("test", obs.NewSimClock(base))

	write := func(n int) {
		b, err := NewWALBus(WALOptions{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if err := b.Publish(context.Background(), TopicBars, env(t, builder, TopicBars, "AAPL", struct{ N int }{i})); err != nil {
				t.Fatal(err)
			}
		}
		_ = b.Close()
	}
	consume := func(limit int) int {
		b, err := NewWALBus(WALOptions{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		// The handler runs on the bus's consumer goroutine while this one
		// polls, so the count is shared across goroutines.
		var seen atomic.Int64
		if err := b.Subscribe(TopicBars, "group", func(context.Context, Envelope) error {
			seen.Add(1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		go func() { _ = b.Run(ctx) }()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && seen.Load() < int64(limit) {
			time.Sleep(5 * time.Millisecond)
		}
		return int(seen.Load())
	}

	write(20)
	if n := consume(20); n != 20 {
		t.Fatalf("first pass consumed %d of 20", n)
	}
	// Nothing new: a resumed group must not replay what it already committed.
	if n := consume(0); n != 0 {
		t.Fatalf("the group replayed %d already-committed events", n)
	}
	write(5)
	if n := consume(5); n != 5 {
		t.Fatalf("second pass consumed %d of 5 new events", n)
	}
}

func TestWALRecoversFromATornTrailingRecord(t *testing.T) {
	dir := t.TempDir()
	builder := NewBuilder("test", obs.NewSimClock(base))
	b, err := NewWALBus(WALOptions{Dir: dir, SyncEveryWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := b.Publish(context.Background(), TopicBars, env(t, builder, TopicBars, "AAPL", struct{ N int }{i})); err != nil {
			t.Fatal(err)
		}
	}
	_ = b.Close()

	// Simulate a crash mid-write by appending a partial record.
	seg := dir + "/market.bars/00000000000000000000.log"
	f, err := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x50, 0x00, 0x00, 0x00, 't', 'o', 'r', 'n'}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	b2, err := NewWALBus(WALOptions{Dir: dir})
	if err != nil {
		t.Fatalf("the log did not recover from a torn tail: %v", err)
	}
	defer b2.Close()
	if d := b2.Depth(TopicBars); d != 10 {
		t.Fatalf("recovered depth is %d, expected the ten intact records", d)
	}
}

func TestTopicConfigsCoverEveryTopic(t *testing.T) {
	byName := map[string]bool{}
	for _, c := range TopicConfigs {
		byName[c.Name] = true
		if c.Partitions <= 0 {
			t.Fatalf("topic %s has %d partitions", c.Name, c.Partitions)
		}
		if c.Retention <= 0 {
			t.Fatalf("topic %s has no retention", c.Name)
		}
	}
	for _, name := range AllTopics {
		if !byName[name] {
			t.Fatalf("topic %s has no operational specification", name)
		}
	}
	if len(TopicConfigs) != len(AllTopics) {
		t.Fatalf("%d configs for %d topics", len(TopicConfigs), len(AllTopics))
	}
}

func TestEnvelopeRoundTripsThroughJSON(t *testing.T) {
	builder := NewBuilder("test", obs.NewSimClock(base))
	e := env(t, builder, TopicSignal, "sig", map[string]any{"side": "LONG", "strength": 72.5})
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.EventID != e.EventID || back.Type != e.Type || back.SchemaVersion != e.SchemaVersion {
		t.Fatal("the envelope did not survive a JSON round trip")
	}
	var payload map[string]any
	if err := back.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["side"] != "LONG" {
		t.Fatal("the payload did not survive")
	}
}
