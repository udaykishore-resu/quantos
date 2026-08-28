package bus

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

// NewEventID returns a UUIDv7-shaped identifier: 48 bits of millisecond
// timestamp followed by randomness. Time-ordered ids keep the dedup index and
// the WAL segment lookups efficient, and make an id sortable by creation time.
func NewEventID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UTC().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		n := atomic.AddUint64(&idFallback, 1)
		binary.BigEndian.PutUint64(b[8:], n)
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

var idFallback uint64

// Builder constructs envelopes with consistent producer identity and trace
// propagation. Every service holds one.
type Builder struct {
	Producer string
	Clock    obs.Clock
}

// NewBuilder returns a Builder for a named producer.
func NewBuilder(producer string, clock obs.Clock) *Builder {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return &Builder{Producer: producer, Clock: clock}
}

// New builds an envelope, inheriting trace, correlation and causation from ctx
// and from the causing envelope when supplied.
func (b *Builder) New(ctx context.Context, typ, partitionKey string, occurredAt time.Time, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope %s: marshal payload: %w", typ, err)
	}
	now := b.Clock.Now()
	if occurredAt.IsZero() {
		occurredAt = now
	}
	corr := obs.CorrelationID(ctx)
	if corr == "" {
		corr = obs.TraceID(ctx)
	}
	return Envelope{
		EventID:       NewEventID(now),
		Type:          typ,
		SchemaVersion: EnvelopeSchemaVersion,
		OccurredAt:    occurredAt.UTC(),
		ProducedAt:    now,
		TraceID:       obs.TraceID(ctx),
		SpanID:        obs.SpanID(ctx),
		CorrelationID: corr,
		CausationID:   causationFrom(ctx),
		PartitionKey:  partitionKey,
		Producer:      b.Producer,
		Payload:       raw,
	}, nil
}

// Caused builds an envelope whose causation chain points at parent.
func (b *Builder) Caused(ctx context.Context, parent Envelope, typ, partitionKey string, occurredAt time.Time, payload any) (Envelope, error) {
	e, err := b.New(ctx, typ, partitionKey, occurredAt, payload)
	if err != nil {
		return Envelope{}, err
	}
	e.CausationID = parent.EventID
	if parent.CorrelationID != "" {
		e.CorrelationID = parent.CorrelationID
	}
	if e.TraceID == "" {
		e.TraceID = parent.TraceID
	}
	return e, nil
}

type causationKey struct{}

// WithCausation records the event id that a downstream emission was caused by.
func WithCausation(ctx context.Context, eventID string) context.Context {
	return context.WithValue(ctx, causationKey{}, eventID)
}

func causationFrom(ctx context.Context) string {
	if v, ok := ctx.Value(causationKey{}).(string); ok {
		return v
	}
	return ""
}

// ContextFor derives a consumer-side context carrying the envelope's trace,
// correlation and causation identity. Handlers should always start from this so
// that emissions they cause are linked.
func ContextFor(ctx context.Context, e Envelope) context.Context {
	if e.TraceID != "" {
		ctx = obs.WithTrace(ctx, e.TraceID, e.SpanID)
	}
	if e.CorrelationID != "" {
		ctx = obs.WithCorrelationID(ctx, e.CorrelationID)
	}
	return WithCausation(ctx, e.EventID)
}
