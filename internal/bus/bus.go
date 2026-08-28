// Package bus is the event backbone abstraction described in ADR-010.
//
// It defines the envelope every QuantOS event travels in, the canonical topic
// names, and the Publisher/Subscriber interfaces. Two drivers ship in-tree:
//
//	memory — in-process fan-out preserving per-key ordering, used by tests,
//	         the backtester and `make demo`.
//	wal    — a durable append-only segment log on local disk with consumer
//	         groups, committed offsets and replay.
//
// A Kafka driver lives in drivers/kafka as a separate module so that the core
// build has no third-party broker dependency; see drivers/kafka/README.md.
package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Canonical topics. These names are contract; changing one is a breaking change
// requiring a schema-version bump and a migration plan.
const (
	TopicQuotes            = "market.quotes"
	TopicTrades            = "market.trades"
	TopicBars              = "market.bars"
	TopicNews              = "market.news"
	TopicEvents            = "market.events"
	TopicStale             = "market.stale"
	TopicRejected          = "market.rejected"
	TopicFeatures          = "features.updated"
	TopicRegime            = "regime.updated"
	TopicPrediction        = "prediction.generated"
	TopicSignal            = "signal.generated"
	TopicSignalInvalidated = "signal.invalidated"
	TopicRisk              = "risk.updated"
	TopicAlert             = "alert.generated"
	TopicEvaluated         = "prediction.evaluated"
	TopicDrift             = "model.drift.detected"
)

// AllTopics is used for provisioning and for the memory driver's fan-out map.
var AllTopics = []string{
	TopicQuotes, TopicTrades, TopicBars, TopicNews, TopicEvents, TopicStale,
	TopicRejected, TopicFeatures, TopicRegime, TopicPrediction, TopicSignal,
	TopicSignalInvalidated, TopicRisk, TopicAlert, TopicEvaluated, TopicDrift,
}

// TopicConfig captures the operational parameters of a topic. It is used by
// provisioning (Terraform/Helm render it) and documented in ADR-002.
type TopicConfig struct {
	Name       string
	Partitions int
	Retention  time.Duration
	Compact    bool
}

// TopicConfigs is the authoritative topic specification.
var TopicConfigs = []TopicConfig{
	{TopicQuotes, 12, 24 * time.Hour, false},
	{TopicTrades, 12, 24 * time.Hour, false},
	{TopicBars, 12, 30 * 24 * time.Hour, false},
	{TopicNews, 6, 30 * 24 * time.Hour, false},
	{TopicEvents, 3, 90 * 24 * time.Hour, false},
	{TopicStale, 3, 7 * 24 * time.Hour, false},
	{TopicRejected, 3, 7 * 24 * time.Hour, false},
	{TopicFeatures, 12, 7 * 24 * time.Hour, false},
	{TopicRegime, 1, 90 * 24 * time.Hour, false},
	{TopicPrediction, 12, 30 * 24 * time.Hour, false},
	{TopicSignal, 6, 365 * 24 * time.Hour, true},
	{TopicSignalInvalidated, 6, 365 * 24 * time.Hour, false},
	{TopicRisk, 6, 30 * 24 * time.Hour, false},
	{TopicAlert, 6, 90 * 24 * time.Hour, false},
	{TopicEvaluated, 6, 365 * 24 * time.Hour, false},
	{TopicDrift, 1, 365 * 24 * time.Hour, false},
}

// EnvelopeSchemaVersion is bumped only for breaking envelope changes.
const EnvelopeSchemaVersion = 1

// Envelope wraps every event with the identity and causality metadata that
// makes the platform traceable and idempotent (ADR-010).
type Envelope struct {
	EventID       string          `json:"event_id"`
	Type          string          `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	ProducedAt    time.Time       `json:"produced_at"`
	TraceID       string          `json:"trace_id,omitempty"`
	SpanID        string          `json:"span_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	PartitionKey  string          `json:"partition_key"`
	Producer      string          `json:"producer,omitempty"`
	Payload       json.RawMessage `json:"payload"`

	// Offset and Partition are set by the driver on delivery; they are not
	// part of the producer-side contract.
	Offset    int64 `json:"-"`
	Partition int   `json:"-"`
}

// Validate enforces the envelope invariants that consumers rely on.
func (e Envelope) Validate() error {
	switch {
	case e.EventID == "":
		return errors.New("envelope: empty event_id")
	case e.Type == "":
		return errors.New("envelope: empty type")
	case e.OccurredAt.IsZero():
		return errors.New("envelope: zero occurred_at")
	case len(e.Payload) == 0:
		return errors.New("envelope: empty payload")
	case e.SchemaVersion <= 0:
		return errors.New("envelope: schema_version must be positive")
	}
	return nil
}

// Age returns how old the event is relative to now, in domain time.
func (e Envelope) Age(now time.Time) time.Duration { return now.Sub(e.OccurredAt) }

// Decode unmarshals the payload into v.
func (e Envelope) Decode(v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("envelope %s: decode %s: %w", e.EventID, e.Type, err)
	}
	return nil
}

// Handler processes one event. Returning an error causes the driver to retry
// according to its policy and eventually route to the dead-letter topic.
type Handler func(ctx context.Context, e Envelope) error

// Publisher emits events.
type Publisher interface {
	Publish(ctx context.Context, topic string, e Envelope) error
	// PublishBatch is not merely a loop: drivers may use it to amortise
	// round-trips, and it is atomic per partition where the driver supports it.
	PublishBatch(ctx context.Context, topic string, es []Envelope) error
	Close() error
}

// Subscriber consumes events. Subscribe registers a handler for a topic under a
// consumer group; Run drives delivery until ctx is cancelled.
type Subscriber interface {
	Subscribe(topic, group string, h Handler) error
	Run(ctx context.Context) error
	Close() error
}

// Bus is the combined interface most components take.
type Bus interface {
	Publisher
	Subscriber
}

// ErrClosed is returned by drivers after Close.
var ErrClosed = errors.New("bus: closed")

// ErrUnknownTopic is returned when publishing to an unregistered topic.
var ErrUnknownTopic = errors.New("bus: unknown topic")
