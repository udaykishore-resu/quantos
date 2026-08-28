package obs

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is a minimal, allocation-conscious metrics registry that exposes the
// Prometheus text exposition format (version 0.0.4).
//
// Writing this rather than vendoring the upstream client keeps the dependency
// surface small and makes the exposition semantics explicit. It supports the
// three instrument types QuantOS needs: counters, gauges and histograms, all
// with labels.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*metricFamily
	order   []string
}

type metricKind int

const (
	kindCounter metricKind = iota
	kindGauge
	kindHistogram
)

type metricFamily struct {
	name    string
	help    string
	kind    metricKind
	labels  []string
	buckets []float64

	mu     sync.RWMutex
	series map[string]*series
	keys   []string
}

type series struct {
	labelValues []string
	// counters and gauges use bits for lock-free float storage
	value atomic.Uint64
	// histogram state
	counts []atomic.Uint64
	sum    atomic.Uint64
	total  atomic.Uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{metrics: map[string]*metricFamily{}}
}

// DefaultBuckets are latency buckets in seconds, chosen around the SLO targets
// in docs/operations/slo.md (100 ms, 300 ms, 500 ms, 1 s).
var DefaultBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5, 10,
}

func (r *Registry) register(name, help string, kind metricKind, labels []string, buckets []float64) *metricFamily {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.metrics[name]; ok {
		return f
	}
	f := &metricFamily{
		name:    name,
		help:    help,
		kind:    kind,
		labels:  append([]string(nil), labels...),
		buckets: buckets,
		series:  map[string]*series{},
	}
	r.metrics[name] = f
	r.order = append(r.order, name)
	sort.Strings(r.order)
	return f
}

// Counter is a monotonically increasing metric.
type Counter struct{ f *metricFamily }

// Gauge is a metric that can go up and down.
type Gauge struct{ f *metricFamily }

// Histogram observes a distribution of values.
type Histogram struct{ f *metricFamily }

// NewCounter registers (or returns) a counter.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{r.register(name, help, kindCounter, labels, nil)}
}

// NewGauge registers (or returns) a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.register(name, help, kindGauge, labels, nil)}
}

// NewHistogram registers (or returns) a histogram with explicit buckets.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Histogram{r.register(name, help, kindHistogram, labels, b)}
}

func (f *metricFamily) get(values []string) *series {
	key := strings.Join(values, "\x00")
	f.mu.RLock()
	s, ok := f.series[key]
	f.mu.RUnlock()
	if ok {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok = f.series[key]; ok {
		return s
	}
	s = &series{labelValues: append([]string(nil), values...)}
	if f.kind == kindHistogram {
		s.counts = make([]atomic.Uint64, len(f.buckets))
	}
	f.series[key] = s
	f.keys = append(f.keys, key)
	sort.Strings(f.keys)
	return s
}

func addFloat(a *atomic.Uint64, delta float64) {
	for {
		old := a.Load()
		nv := math.Float64frombits(old) + delta
		if a.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

// Inc increments the counter by one.
func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

// Add increments the counter by delta. Negative deltas are ignored, since a
// counter that goes backwards silently corrupts rate() calculations.
func (c *Counter) Add(delta float64, labels ...string) {
	if delta < 0 || math.IsNaN(delta) {
		return
	}
	addFloat(&c.f.get(labels).value, delta)
}

// Set assigns the gauge value.
func (g *Gauge) Set(v float64, labels ...string) {
	g.f.get(labels).value.Store(math.Float64bits(v))
}

// Add adds delta to the gauge.
func (g *Gauge) Add(delta float64, labels ...string) { addFloat(&g.f.get(labels).value, delta) }

// Inc increments the gauge.
func (g *Gauge) Inc(labels ...string) { g.Add(1, labels...) }

// Dec decrements the gauge.
func (g *Gauge) Dec(labels ...string) { g.Add(-1, labels...) }

// Observe records one sample.
func (h *Histogram) Observe(v float64, labels ...string) {
	if math.IsNaN(v) {
		return
	}
	s := h.f.get(labels)
	for i, b := range h.f.buckets {
		if v <= b {
			s.counts[i].Add(1)
		}
	}
	addFloat(&s.sum, v)
	s.total.Add(1)
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

func labelPairs(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)+len(extra)/2)
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts = append(parts, fmt.Sprintf("%s=%q", n, escapeLabel(v)))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", extra[i], escapeLabel(extra[i+1])))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func fmtFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Gather renders the registry in Prometheus text exposition format.
func (r *Registry) Gather() string {
	r.mu.RLock()
	names := append([]string(nil), r.order...)
	families := make([]*metricFamily, 0, len(names))
	for _, n := range names {
		families = append(families, r.metrics[n])
	}
	r.mu.RUnlock()

	var b strings.Builder
	for _, f := range families {
		f.mu.RLock()
		keys := append([]string(nil), f.keys...)
		f.mu.RUnlock()
		if len(keys) == 0 {
			continue
		}
		if f.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", f.name, f.help)
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.name, typeName(f.kind))
		for _, k := range keys {
			f.mu.RLock()
			s := f.series[k]
			f.mu.RUnlock()
			switch f.kind {
			case kindHistogram:
				cumulative := uint64(0)
				for i, bound := range f.buckets {
					cumulative = s.counts[i].Load()
					fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name,
						labelPairs(f.labels, s.labelValues, "le", fmtFloat(bound)), cumulative)
				}
				total := s.total.Load()
				fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name,
					labelPairs(f.labels, s.labelValues, "le", "+Inf"), total)
				fmt.Fprintf(&b, "%s_sum%s %s\n", f.name,
					labelPairs(f.labels, s.labelValues), fmtFloat(math.Float64frombits(s.sum.Load())))
				fmt.Fprintf(&b, "%s_count%s %d\n", f.name,
					labelPairs(f.labels, s.labelValues), total)
			default:
				fmt.Fprintf(&b, "%s%s %s\n", f.name,
					labelPairs(f.labels, s.labelValues), fmtFloat(math.Float64frombits(s.value.Load())))
			}
		}
	}
	return b.String()
}

func typeName(k metricKind) string {
	switch k {
	case kindCounter:
		return "counter"
	case kindGauge:
		return "gauge"
	default:
		return "histogram"
	}
}

// Handler serves the metrics endpoint.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Gather()))
	})
}

// Metrics is the platform's named instrument set. Holding them in one struct
// means metric names are declared once and cannot drift between services.
type Metrics struct {
	Registry *Registry

	MarketEventsProcessed *Counter
	MarketEventsRejected  *Counter
	MarketEventLatency    *Histogram
	MarketDataStale       *Gauge

	FeatureSnapshots   *Counter
	FeatureLatency     *Histogram
	PredictionLatency  *Histogram
	PredictionsTotal   *Counter
	SignalLatency      *Histogram
	SignalsGenerated   *Counter
	SignalsInvalidated *Counter
	RiskDecisions      *Counter
	AlertsGenerated    *Counter
	AlertsSuppressed   *Counter
	AlertLatency       *Histogram

	PredictionAccuracy *Gauge
	PredictionBrier    *Gauge
	ModelDrift         *Gauge
	ModelLoadFailures  *Counter

	BusPublished  *Counter
	BusConsumed   *Counter
	BusDuplicates *Counter
	BusErrors     *Counter
	BusLag        *Gauge
	BusWALDepth   *Gauge

	HTTPRequests *Counter
	HTTPLatency  *Histogram
	HTTPInFlight *Gauge
	RateLimited  *Counter

	LLMCalls         *Counter
	LLMFailures      *Counter
	LLMLatency       *Histogram
	GroundingRejects *Counter

	DegradedComponents *Gauge
	CircuitState       *Gauge
}

// NewMetrics registers every platform instrument on a fresh registry.
func NewMetrics() *Metrics {
	r := NewRegistry()
	return &Metrics{
		Registry: r,

		MarketEventsProcessed: r.NewCounter("quantos_market_events_processed_total", "Market events accepted and published.", "type", "provider"),
		MarketEventsRejected:  r.NewCounter("quantos_market_events_rejected_total", "Market events rejected by validation.", "type", "reason"),
		MarketEventLatency:    r.NewHistogram("quantos_market_event_processing_duration_seconds", "Ingest to features.updated latency.", DefaultBuckets, "type"),
		MarketDataStale:       r.NewGauge("quantos_market_data_stale", "1 when a symbol's data is considered stale.", "ticker"),

		FeatureSnapshots:   r.NewCounter("quantos_feature_snapshots_total", "Feature snapshots produced.", "ticker"),
		FeatureLatency:     r.NewHistogram("quantos_feature_computation_duration_seconds", "Feature computation latency.", DefaultBuckets),
		PredictionLatency:  r.NewHistogram("quantos_prediction_latency_seconds", "Feature update to prediction latency.", DefaultBuckets, "model"),
		PredictionsTotal:   r.NewCounter("quantos_predictions_total", "Predictions generated.", "model", "source"),
		SignalLatency:      r.NewHistogram("quantos_signal_generation_latency_seconds", "Prediction to signal latency.", DefaultBuckets),
		SignalsGenerated:   r.NewCounter("quantos_signals_generated_total", "Paper-trading signals generated.", "strategy", "side"),
		SignalsInvalidated: r.NewCounter("quantos_signals_invalidated_total", "Signals invalidated.", "kind"),
		RiskDecisions:      r.NewCounter("quantos_risk_decisions_total", "Risk engine verdicts.", "decision", "reason"),
		AlertsGenerated:    r.NewCounter("quantos_alerts_generated_total", "Alerts emitted.", "type", "severity"),
		AlertsSuppressed:   r.NewCounter("quantos_alerts_suppressed_total", "Alerts suppressed by dedup or cooldown.", "type", "reason"),
		AlertLatency:       r.NewHistogram("quantos_alert_generation_latency_seconds", "Condition to alert latency.", DefaultBuckets, "type"),

		PredictionAccuracy: r.NewGauge("quantos_prediction_accuracy", "Rolling prediction accuracy.", "model", "horizon", "regime"),
		PredictionBrier:    r.NewGauge("quantos_prediction_brier_score", "Rolling Brier score.", "model", "horizon"),
		ModelDrift:         r.NewGauge("quantos_model_drift", "Drift severity as an ordinal 0..3.", "model", "kind"),
		ModelLoadFailures:  r.NewCounter("quantos_model_artifact_load_failures_total", "Model artifact load failures.", "model", "reason"),

		BusPublished:  r.NewCounter("quantos_bus_published_total", "Events published.", "topic"),
		BusConsumed:   r.NewCounter("quantos_bus_consumed_total", "Events consumed.", "topic", "group"),
		BusDuplicates: r.NewCounter("quantos_bus_duplicates_total", "Duplicate events suppressed by idempotency.", "topic", "group"),
		BusErrors:     r.NewCounter("quantos_bus_errors_total", "Bus errors.", "topic", "op"),
		BusLag:        r.NewGauge("quantos_kafka_lag", "Consumer lag in events.", "topic", "group"),
		BusWALDepth:   r.NewGauge("quantos_bus_wal_depth", "Buffered events awaiting publish.", "topic"),

		HTTPRequests: r.NewCounter("quantos_http_requests_total", "HTTP requests.", "method", "route", "status"),
		HTTPLatency:  r.NewHistogram("quantos_http_server_duration_seconds", "HTTP handler latency.", DefaultBuckets, "method", "route"),
		HTTPInFlight: r.NewGauge("quantos_http_in_flight_requests", "In-flight HTTP requests."),
		RateLimited:  r.NewCounter("quantos_rate_limited_total", "Requests rejected by the rate limiter.", "route", "principal"),

		LLMCalls:         r.NewCounter("quantos_llm_calls_total", "LLM invocations.", "provider", "purpose"),
		LLMFailures:      r.NewCounter("quantos_llm_failures_total", "LLM invocation failures.", "provider", "reason"),
		LLMLatency:       r.NewHistogram("quantos_llm_duration_seconds", "LLM call latency.", []float64{0.1, 0.5, 1, 2, 5, 10, 20, 30}, "provider"),
		GroundingRejects: r.NewCounter("quantos_analyst_grounding_rejections_total", "Generated narratives rejected by the grounding validator.", "reason"),

		DegradedComponents: r.NewGauge("quantos_degraded_component", "1 when a component is degraded.", "component"),
		CircuitState:       r.NewGauge("quantos_circuit_state", "Circuit breaker state: 0 closed, 1 half-open, 2 open.", "name"),
	}
}
