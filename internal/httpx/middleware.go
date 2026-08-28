package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/auth"
	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

type ctxKey int

const ctxKeyPrincipal ctxKey = iota

// PrincipalFrom returns the authenticated principal on the context.
func PrincipalFrom(ctx context.Context) auth.Principal {
	if p, ok := ctx.Value(ctxKeyPrincipal).(auth.Principal); ok {
		return p
	}
	return auth.Principal{}
}

// statusWriter records the status code and byte count for logging and metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush forwards to the underlying writer, which the SSE handler needs.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Correlation assigns a request id and propagates W3C trace context, so every
// log line, metric exemplar and emitted event on this request's causal chain
// shares an identity (requirement §34).
func Correlation(tracer *obs.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := r.Header.Get("X-Request-ID")
			if reqID == "" {
				reqID = obs.NewSpanID()
			}
			ctx := obs.WithRequestID(r.Context(), reqID)

			if tid, sid, ok := obs.ParseTraceParent(r.Header.Get("traceparent")); ok {
				ctx = obs.WithTrace(ctx, tid, sid)
			}
			corr := r.Header.Get("X-Correlation-ID")
			if corr == "" {
				corr = reqID
			}
			ctx = obs.WithCorrelationID(ctx, corr)

			var span *obs.Span
			if tracer != nil {
				ctx, span = tracer.Start(ctx, r.Method+" "+r.URL.Path, obs.SpanServer)
				span.SetAttr("http.method", r.Method).
					SetAttr("http.route", r.URL.Path).
					SetAttr("request.id", reqID)
				defer span.End()
			}

			w.Header().Set("X-Request-ID", reqID)
			w.Header().Set("X-Correlation-ID", corr)
			if tp := obs.TraceParent(ctx); tp != "" {
				w.Header().Set("traceparent", tp)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Logging emits one structured line per request.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			lvl := slog.LevelInfo
			switch {
			case sw.status >= 500:
				lvl = slog.LevelError
			case sw.status >= 400:
				lvl = slog.LevelWarn
			}
			log.Log(r.Context(), lvl, "http request",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr, "user_agent", r.UserAgent())
		})
	}
}

// Metrics records request counts, latency and in-flight gauge.
func Metrics(m *obs.Metrics, routeOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil {
				next.ServeHTTP(w, r)
				return
			}
			route := r.URL.Path
			if routeOf != nil {
				route = routeOf(r)
			}
			m.HTTPInFlight.Inc()
			defer m.HTTPInFlight.Dec()
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			m.HTTPLatency.Observe(time.Since(start).Seconds(), r.Method, route)
			m.HTTPRequests.Inc(r.Method, route, fmt.Sprint(sw.status))
		})
	}
}

// Recoverer converts a panic into a 500 without taking the process down, and
// logs the stack with the request id so it is findable.
func Recoverer(m *obs.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.ErrorContext(r.Context(), "panic in handler",
						"panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
						"path", r.URL.Path)
					if m != nil {
						m.HTTPRequests.Inc(r.Method, r.URL.Path, "500")
					}
					Fail(w, r, ErrInternal)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORS applies a strict allow-list. A wildcard origin is never emitted when
// credentials are allowed, because that combination is what turns a
// convenience into a vulnerability.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID, traceparent")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets the baseline response headers.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		// The API serves JSON only; a restrictive CSP costs nothing here and
		// removes a class of content-sniffing surprises.
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// Authenticate verifies the bearer token and attaches the principal.
//
// allowQueryToken exists for the SSE endpoint: EventSource cannot set headers,
// so the stream route accepts a token as a query parameter. It is enabled per
// route rather than globally, because a token in a URL ends up in access logs.
func Authenticate(a *auth.Authenticator, allowQueryToken bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a == nil || !a.Enabled() {
				ctx := context.WithValue(r.Context(), ctxKeyPrincipal, auth.Anonymous())
				next.ServeHTTP(w, r.WithContext(obs.WithPrincipal(ctx, "anonymous")))
				return
			}
			token := auth.BearerToken(r)
			if token == "" && allowQueryToken {
				token = r.URL.Query().Get("access_token")
			}
			p, err := a.Verify(r.Context(), token)
			if err != nil {
				Fail(w, r, ErrUnauthorized.WithDetail(err.Error()))
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
			ctx = obs.WithPrincipal(ctx, p.Subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope enforces authorisation.
func RequireScope(s auth.Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if !p.Has(s) {
				Fail(w, r, ErrForbidden.WithDetail("required scope: "+string(s)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Limiter is the interface the rate-limiting middleware needs; store.Cache
// satisfies it, and localLimiter is the in-process fallback.
type Limiter interface {
	Allow(ctx context.Context, principal string, rate float64, burst int, now time.Time, failOpen bool) (bool, error)
}

// localLimiter is an in-process token bucket used when Redis is not configured.
type localLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	clock   obs.Clock
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLocalLimiter builds an in-process rate limiter.
func NewLocalLimiter(clock obs.Clock) Limiter {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return &localLimiter{buckets: map[string]*bucket{}, clock: clock}
}

// Allow implements Limiter.
func (l *localLimiter) Allow(_ context.Context, principal string, rate float64, burst int, now time.Time, _ bool) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[principal]
	if !ok {
		b = &bucket{tokens: float64(burst), last: now}
		l.buckets[principal] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
}

// RateLimit applies a per-principal token bucket.
//
// The fail direction depends on the method: a read failing open keeps the
// dashboard usable during a Redis outage, while a write failing closed prevents
// an outage from becoming an unmetered write channel.
func RateLimit(l Limiter, rate float64, burst int, clock obs.Clock, m *obs.Metrics) func(http.Handler) http.Handler {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l == nil || rate <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			// The bucket is per principal where there is one, and per client
			// address otherwise. Ordering matters and is easy to get wrong:
			// registered above Authenticate this middleware sees no principal
			// and silently degrades to address-keyed limiting, which shares one
			// bucket across every user behind a proxy. Mount it inside the
			// authenticated group.
			key := PrincipalFrom(r.Context()).Subject
			if key == "" {
				key = clientAddr(r)
			}
			failOpen := r.Method == http.MethodGet || r.Method == http.MethodHead
			ok, err := l.Allow(r.Context(), key, rate, burst, clock.Now(), failOpen)
			if err != nil && !failOpen {
				Fail(w, r, ErrUnavailable.WithDetail("rate limiter unavailable; write rejected"))
				return
			}
			if !ok {
				if m != nil {
					m.RateLimited.Inc(r.URL.Path, key)
				}
				Fail(w, r, ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientAddr is the address a rate-limit bucket is keyed on when there is no
// principal. It prefers the forwarded address so that a deployment behind a
// load balancer does not put every client in one bucket, and falls back to the
// socket address, which is what a direct connection gives.
func clientAddr(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Audit records state-changing requests to the append-only audit log.
// AuditSink is the append-only audit record the middleware writes to. The
// interface is declared here, on the consuming side, so the HTTP layer depends
// on the behaviour it needs rather than on a storage implementation.
type AuditSink interface {
	Append(ctx context.Context, e domain.AuditEvent) error
}

func Audit(s AuditSink, clock obs.Clock) func(http.Handler) http.Handler {
	if clock == nil {
		clock = obs.SystemClock{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s == nil || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			p := PrincipalFrom(r.Context())
			outcome := "success"
			if sw.status >= 400 {
				outcome = "failure"
			}
			_ = s.Append(r.Context(), domain.AuditEvent{
				ID:        bus.NewEventID(clock.Now()),
				At:        clock.Now(),
				Principal: orDefault(p.Subject, "anonymous"),
				Action:    r.Method + " " + r.URL.Path,
				Resource:  r.URL.Path,
				Outcome:   outcome,
				RequestID: obs.RequestID(r.Context()),
				TraceID:   obs.TraceID(r.Context()),
				RemoteIP:  r.RemoteAddr,
				Detail:    map[string]string{"status": fmt.Sprint(sw.status)},
			})
		})
	}
}

// MaxBody bounds request bodies.
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
