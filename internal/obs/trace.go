package obs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tracer produces spans and exports them over OTLP/HTTP with JSON encoding,
// which every OpenTelemetry Collector accepts on /v1/traces.
//
// Implementing the exporter directly rather than vendoring the SDK keeps the
// dependency surface small; the wire format is a stable, versioned contract.
type Tracer struct {
	service  string
	version  string
	env      string
	endpoint string
	client   *http.Client
	clock    Clock

	mu      sync.Mutex
	pending []*Span
	stopped bool

	maxBatch   int
	flushEvery time.Duration
	wg         sync.WaitGroup
	stop       chan struct{}
	// Enabled is false when no endpoint is configured; spans are still created
	// (so trace ids propagate) but never exported.
	Enabled bool
}

// TracerConfig parameterises the tracer.
type TracerConfig struct {
	Service    string
	Version    string
	Env        string
	Endpoint   string // e.g. http://otel-collector:4318
	MaxBatch   int
	FlushEvery time.Duration
	Clock      Clock
}

// NewTracer builds a tracer. An empty Endpoint yields a no-export tracer that
// still generates and propagates ids.
func NewTracer(cfg TracerConfig) *Tracer {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 256
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 5 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	t := &Tracer{
		service:    cfg.Service,
		version:    cfg.Version,
		env:        cfg.Env,
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		client:     &http.Client{Timeout: 5 * time.Second},
		clock:      cfg.Clock,
		maxBatch:   cfg.MaxBatch,
		flushEvery: cfg.FlushEvery,
		stop:       make(chan struct{}),
		Enabled:    cfg.Endpoint != "",
	}
	if t.Enabled {
		t.wg.Add(1)
		go t.loop()
	}
	return t
}

func (t *Tracer) loop() {
	defer t.wg.Done()
	tick := time.NewTicker(t.flushEvery)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			t.flush()
			return
		case <-tick.C:
			t.flush()
		}
	}
}

// Close flushes and stops the exporter.
func (t *Tracer) Close() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	t.mu.Unlock()
	if t.Enabled {
		close(t.stop)
		t.wg.Wait()
	}
}

// SpanKind mirrors the OTLP span kind enum.
type SpanKind int

const (
	SpanInternal SpanKind = 1
	SpanServer   SpanKind = 2
	SpanClient   SpanKind = 3
	SpanProducer SpanKind = 4
	SpanConsumer SpanKind = 5
)

// Span is one unit of traced work.
type Span struct {
	TraceID  string
	SpanID   string
	ParentID string
	Name     string
	Kind     SpanKind
	Start    time.Time
	EndTime  time.Time
	Attrs    map[string]any
	Status   string
	Err      string

	tracer *Tracer
	mu     sync.Mutex
	ended  bool
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Falls back to a time-derived value; ids must never be empty because
		// correlation depends on them.
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i % 8 * 8))
		}
	}
	return hex.EncodeToString(b)
}

// NewTraceID returns a fresh 16-byte trace id.
func NewTraceID() string { return randHex(16) }

// NewSpanID returns a fresh 8-byte span id.
func NewSpanID() string { return randHex(8) }

// Start begins a span, inheriting trace and parent ids from ctx when present.
func (t *Tracer) Start(ctx context.Context, name string, kind SpanKind) (context.Context, *Span) {
	traceID := TraceID(ctx)
	if traceID == "" {
		traceID = NewTraceID()
	}
	s := &Span{
		TraceID:  traceID,
		SpanID:   NewSpanID(),
		ParentID: SpanID(ctx),
		Name:     name,
		Kind:     kind,
		Start:    t.clock.Now(),
		Attrs:    map[string]any{},
		Status:   "UNSET",
		tracer:   t,
	}
	return WithTrace(ctx, s.TraceID, s.SpanID), s
}

// SetAttr attaches an attribute to the span.
func (s *Span) SetAttr(k string, v any) *Span {
	if s == nil {
		return s
	}
	s.mu.Lock()
	s.Attrs[k] = v
	s.mu.Unlock()
	return s
}

// RecordError marks the span as failed.
func (s *Span) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.Status = "ERROR"
	s.Err = err.Error()
	s.mu.Unlock()
}

// End completes the span and queues it for export. It is idempotent.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	if s.tracer != nil {
		s.EndTime = s.tracer.clock.Now()
	} else {
		s.EndTime = time.Now().UTC()
	}
	if s.Status == "UNSET" {
		s.Status = "OK"
	}
	t := s.tracer
	s.mu.Unlock()
	if t != nil {
		t.enqueue(s)
	}
}

// Duration returns the span's elapsed time, or 0 while still open.
func (s *Span) Duration() time.Duration {
	if s == nil || s.EndTime.IsZero() {
		return 0
	}
	return s.EndTime.Sub(s.Start)
}

func (t *Tracer) enqueue(s *Span) {
	if !t.Enabled {
		return
	}
	t.mu.Lock()
	t.pending = append(t.pending, s)
	n := len(t.pending)
	t.mu.Unlock()
	if n >= t.maxBatch {
		go t.flush()
	}
}

func (t *Tracer) flush() {
	t.mu.Lock()
	if len(t.pending) == 0 {
		t.mu.Unlock()
		return
	}
	batch := t.pending
	t.pending = nil
	t.mu.Unlock()

	body, err := t.encode(batch)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, t.endpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		// Tracing must never take the service down; drop the batch.
		return
	}
	_ = resp.Body.Close()
}

// encode renders spans as an OTLP/JSON ExportTraceServiceRequest.
func (t *Tracer) encode(spans []*Span) ([]byte, error) {
	type kv struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	}
	mkKV := func(k string, v any) kv {
		var x kv
		x.Key = k
		x.Value.StringValue = fmt.Sprint(v)
		return x
	}
	type otlpSpan struct {
		TraceID           string `json:"traceId"`
		SpanID            string `json:"spanId"`
		ParentSpanID      string `json:"parentSpanId,omitempty"`
		Name              string `json:"name"`
		Kind              int    `json:"kind"`
		StartTimeUnixNano string `json:"startTimeUnixNano"`
		EndTimeUnixNano   string `json:"endTimeUnixNano"`
		Attributes        []kv   `json:"attributes,omitempty"`
		Status            struct {
			Code    int    `json:"code"`
			Message string `json:"message,omitempty"`
		} `json:"status"`
	}
	out := make([]otlpSpan, 0, len(spans))
	for _, s := range spans {
		o := otlpSpan{
			TraceID:           s.TraceID,
			SpanID:            s.SpanID,
			ParentSpanID:      s.ParentID,
			Name:              s.Name,
			Kind:              int(s.Kind),
			StartTimeUnixNano: fmt.Sprint(s.Start.UnixNano()),
			EndTimeUnixNano:   fmt.Sprint(s.EndTime.UnixNano()),
		}
		for k, v := range s.Attrs {
			o.Attributes = append(o.Attributes, mkKV(k, v))
		}
		if s.Status == "ERROR" {
			o.Status.Code = 2
			o.Status.Message = s.Err
		} else {
			o.Status.Code = 1
		}
		out = append(out, o)
	}
	payload := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []kv{
				mkKV("service.name", t.service),
				mkKV("service.version", t.version),
				mkKV("deployment.environment", t.env),
			}},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "quantos"},
				"spans": out,
			}},
		}},
	}
	return json.Marshal(payload)
}

// TraceParent renders the W3C traceparent header for a context.
func TraceParent(ctx context.Context) string {
	tid, sid := TraceID(ctx), SpanID(ctx)
	if tid == "" || sid == "" {
		return ""
	}
	return "00-" + tid + "-" + sid + "-01"
}

// ParseTraceParent extracts trace and span ids from a W3C traceparent header.
func ParseTraceParent(h string) (traceID, spanID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}
