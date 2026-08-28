// Package httpx is the HTTP edge: the REST API, the Server-Sent Events stream,
// and the middleware chain that gives every request an identity, a trace, a
// principal and a rate-limit decision.
//
// The response envelope is uniform and always carries the research disclaimer
// (governance rule G-9) and a `degraded` list, so a partial answer is visibly
// partial rather than quietly wrong.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// Envelope wraps every successful response.
type Envelope struct {
	Data       any       `json:"data"`
	Meta       *Meta     `json:"meta,omitempty"`
	Degraded   []string  `json:"degraded,omitempty"`
	Disclaimer string    `json:"disclaimer"`
	RequestID  string    `json:"request_id,omitempty"`
	ServedAt   time.Time `json:"served_at"`
}

// Meta carries pagination and freshness details.
type Meta struct {
	Count  int `json:"count,omitempty"`
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
	// AsOf is a pointer because `omitempty` does nothing for a time.Time: the
	// zero value is a struct, so a plain field ships "0001-01-01T00:00:00Z" on
	// every response that has no as-of to report, and a client that trusts the
	// tag renders year one.
	AsOf        *time.Time `json:"as_of,omitempty"`
	Stale       bool       `json:"stale,omitempty"`
	StaleReason string     `json:"stale_reason,omitempty"`
}

// At builds a Meta carrying an as-of timestamp, omitting it when the caller has
// no timestamp to report rather than shipping the zero time.
func At(t time.Time) *Meta {
	if t.IsZero() {
		return &Meta{}
	}
	return &Meta{AsOf: &t}
}

// ErrorBody is the uniform error response.
type ErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Detail    string `json:"detail,omitempty"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
	Disclaimer string `json:"disclaimer"`
}

// APIError is an error with an HTTP status and a stable code.
type APIError struct {
	Status  int
	Code    string
	Message string
	Detail  string
}

func (e APIError) Error() string { return e.Code + ": " + e.Message }

// Common errors.
var (
	ErrBadRequest   = APIError{Status: http.StatusBadRequest, Code: "bad_request", Message: "the request could not be understood"}
	ErrUnauthorized = APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "authentication is required"}
	ErrForbidden    = APIError{Status: http.StatusForbidden, Code: "forbidden", Message: "the principal lacks the required scope"}
	ErrNotFound     = APIError{Status: http.StatusNotFound, Code: "not_found", Message: "the resource does not exist"}
	ErrRateLimited  = APIError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests"}
	ErrUnavailable  = APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "a required backing service is unavailable"}
	ErrInternal     = APIError{Status: http.StatusInternalServerError, Code: "internal", Message: "an unexpected error occurred"}
)

// WithDetail returns a copy of the error with a detail message.
func (e APIError) WithDetail(d string) APIError { e.Detail = d; return e }

// ServerConfig parameterises the HTTP server.
type ServerConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64
	CORSOrigins     []string
	RateLimitRPS    float64
	RateLimitBurst  int
	Version         string
	Metrics         *obs.Metrics
	Clock           obs.Clock
}

// Server owns the HTTP listener and its lifecycle.
type Server struct {
	cfg    ServerConfig
	router chi.Router
	http   *http.Server
}

// NewServer builds a server with the standard middleware chain applied.
func NewServer(cfg ServerConfig) *Server {
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 120 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(Recoverer(cfg.Metrics))
	return &Server{cfg: cfg, router: r}
}

// Router exposes the router for route registration.
func (s *Server) Router() chi.Router { return s.router }

// Start begins serving and returns immediately.
func (s *Server) Start() error {
	s.http = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.router,
		ReadTimeout:       s.cfg.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout must not apply to the SSE stream, which is long-lived by
		// design; the stream handler runs on a route group that resets it.
		WriteTimeout: s.cfg.WriteTimeout,
		IdleTimeout:  s.cfg.IdleTimeout,
	}
	ln := make(chan error, 1)
	go func() {
		err := s.http.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			ln <- err
			return
		}
		ln <- nil
	}()
	select {
	case err := <-ln:
		return err
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

// Shutdown drains connections.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()
	return s.http.Shutdown(ctx)
}

// Respond writes a successful envelope.
func Respond(w http.ResponseWriter, r *http.Request, status int, data any, meta *Meta, degraded []string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{
		Data: data, Meta: meta, Degraded: degraded,
		Disclaimer: domain.Disclaimer,
		RequestID:  obs.RequestID(r.Context()),
		ServedAt:   time.Now().UTC(),
	})
}

// Fail writes an error envelope. Unknown errors are reported as internal
// without leaking their text, which is both a security and a support decision:
// the detail goes to the log with the request id attached.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	var api APIError
	if !errors.As(err, &api) {
		api = ErrInternal
	}
	var body ErrorBody
	body.Error.Code = api.Code
	body.Error.Message = api.Message
	body.Error.Detail = api.Detail
	body.Error.RequestID = obs.RequestID(r.Context())
	body.Disclaimer = domain.Disclaimer

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if api.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	if api.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(api.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// DecodeJSON reads a bounded JSON body, rejecting unknown fields so a typo in a
// client request is an error rather than a silently ignored setting.
func DecodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrBadRequest.WithDetail(err.Error())
	}
	if dec.More() {
		return ErrBadRequest.WithDetail("request body must contain a single JSON object")
	}
	return nil
}
