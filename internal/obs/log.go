package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const (
	ctxKeyTrace ctxKey = iota
	ctxKeySpan
	ctxKeyRequest
	ctxKeyCorrelation
	ctxKeyPrincipal
)

// WithTrace stores trace identifiers on the context so that every log line and
// every emitted event carries them (requirement §34).
func WithTrace(ctx context.Context, traceID, spanID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyTrace, traceID)
	return context.WithValue(ctx, ctxKeySpan, spanID)
}

// WithRequestID stores the inbound request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequest, id)
}

// WithCorrelationID stores the causal-chain correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyCorrelation, id)
}

// WithPrincipal stores the authenticated subject.
func WithPrincipal(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, sub)
}

func str(ctx context.Context, k ctxKey) string {
	if v, ok := ctx.Value(k).(string); ok {
		return v
	}
	return ""
}

// TraceID returns the trace id on the context, or "".
func TraceID(ctx context.Context) string { return str(ctx, ctxKeyTrace) }

// SpanID returns the span id on the context, or "".
func SpanID(ctx context.Context) string { return str(ctx, ctxKeySpan) }

// RequestID returns the request id on the context, or "".
func RequestID(ctx context.Context) string { return str(ctx, ctxKeyRequest) }

// CorrelationID returns the correlation id on the context, or "".
func CorrelationID(ctx context.Context) string { return str(ctx, ctxKeyCorrelation) }

// Principal returns the authenticated subject on the context, or "".
func Principal(ctx context.Context) string { return str(ctx, ctxKeyPrincipal) }

// contextHandler injects the correlation fields into every record so that
// callers cannot forget them.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, p := range []struct {
		key string
		k   ctxKey
	}{
		{"trace_id", ctxKeyTrace},
		{"span_id", ctxKeySpan},
		{"request_id", ctxKeyRequest},
		{"correlation_id", ctxKeyCorrelation},
		{"principal", ctxKeyPrincipal},
	} {
		if v := str(ctx, p.k); v != "" {
			r.AddAttrs(slog.String(p.key, v))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(a)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// LogConfig parameterises logger construction.
type LogConfig struct {
	Level   string // debug|info|warn|error
	Format  string // json|text
	Service string
	Version string
	Env     string
}

// NewLogger builds a structured logger. JSON is the default because logs are
// machine-consumed first and human-read second.
func NewLogger(cfg LogConfig) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	h = contextHandler{h}
	l := slog.New(h)
	attrs := []any{}
	if cfg.Service != "" {
		attrs = append(attrs, "service", cfg.Service)
	}
	if cfg.Version != "" {
		attrs = append(attrs, "version", cfg.Version)
	}
	if cfg.Env != "" {
		attrs = append(attrs, "env", cfg.Env)
	}
	if len(attrs) > 0 {
		l = l.With(attrs...)
	}
	return l
}

// Discard returns a logger that drops everything, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
