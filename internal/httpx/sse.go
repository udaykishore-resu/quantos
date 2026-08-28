package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishoreresu/quantos/internal/obs"
)

// StreamEvent is one server-sent event.
type StreamEvent struct {
	ID    uint64    `json:"id"`
	Topic string    `json:"topic"`
	Data  any       `json:"data"`
	At    time.Time `json:"at"`
}

// Hub fans events out to connected dashboards over Server-Sent Events.
//
// SSE rather than WebSocket is a deliberate choice (ADR-004): the dashboard's
// real-time need is strictly server-to-client, and SSE gives automatic
// reconnection with Last-Event-ID resume for free. The hub keeps a bounded ring
// buffer so a client that reconnects within the window resumes without a gap,
// and receives an explicit `resync` instruction when it cannot.
type Hub struct {
	mu       sync.RWMutex
	clients  map[uint64]*client
	nextID   atomic.Uint64
	nextConn atomic.Uint64

	ring     []StreamEvent
	ringSize int
	ringHead int
	ringLen  int

	clock           obs.Clock
	metrics         *obs.Metrics
	perClientBuffer int
}

type client struct {
	id     uint64
	ch     chan StreamEvent
	topics map[string]bool
	closed atomic.Bool
}

// HubConfig parameterises the hub.
type HubConfig struct {
	// ReplaySize is how many recent events are retained for resume.
	ReplaySize int
	// ClientBuffer bounds each connection's queue. On overflow the connection
	// is dropped with an explicit event rather than growing without bound.
	ClientBuffer int
	Clock        obs.Clock
	Metrics      *obs.Metrics
}

// NewHub builds an event hub.
func NewHub(cfg HubConfig) *Hub {
	if cfg.ReplaySize <= 0 {
		cfg.ReplaySize = 1024
	}
	if cfg.ClientBuffer <= 0 {
		cfg.ClientBuffer = 256
	}
	if cfg.Clock == nil {
		cfg.Clock = obs.SystemClock{}
	}
	return &Hub{
		clients: map[uint64]*client{}, ring: make([]StreamEvent, cfg.ReplaySize),
		ringSize: cfg.ReplaySize, clock: cfg.Clock, metrics: cfg.Metrics,
		perClientBuffer: cfg.ClientBuffer,
	}
}

// Publish fans an event out to subscribed clients and records it for resume.
func (h *Hub) Publish(topic string, data any) {
	ev := StreamEvent{ID: h.nextID.Add(1), Topic: topic, Data: data, At: h.clock.Now()}

	h.mu.Lock()
	h.ring[h.ringHead] = ev
	h.ringHead = (h.ringHead + 1) % h.ringSize
	if h.ringLen < h.ringSize {
		h.ringLen++
	}
	targets := make([]*client, 0, len(h.clients))
	for _, c := range h.clients {
		if len(c.topics) == 0 || c.topics[topic] {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	for _, c := range targets {
		select {
		case c.ch <- ev:
		default:
			// Slow consumer: drop the connection rather than the guarantee.
			// A client that cannot keep up reconnects and resyncs.
			if c.closed.CompareAndSwap(false, true) {
				close(c.ch)
			}
		}
	}
}

// replayFrom returns buffered events with an id greater than lastID, and
// whether a full resync is required because the client fell outside the window.
func (h *Hub) replayFrom(lastID uint64) ([]StreamEvent, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.ringLen == 0 {
		return nil, false
	}
	out := make([]StreamEvent, 0, h.ringLen)
	oldest := uint64(0)
	for i := 0; i < h.ringLen; i++ {
		idx := (h.ringHead - h.ringLen + i + h.ringSize) % h.ringSize
		ev := h.ring[idx]
		if oldest == 0 || ev.ID < oldest {
			oldest = ev.ID
		}
		if ev.ID > lastID {
			out = append(out, ev)
		}
	}
	// If the client's last id is older than everything we still hold, it has a
	// gap and must refetch state over REST.
	if lastID > 0 && oldest > lastID+1 {
		return out, true
	}
	return out, false
}

func (h *Hub) add(topics []string) *client {
	c := &client{
		id: h.nextConn.Add(1), ch: make(chan StreamEvent, h.perClientBuffer),
		topics: map[string]bool{},
	}
	for _, t := range topics {
		if t = strings.TrimSpace(t); t != "" {
			c.topics[t] = true
		}
	}
	h.mu.Lock()
	h.clients[c.id] = c
	h.mu.Unlock()
	return c
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c.id)
	h.mu.Unlock()
	if c.closed.CompareAndSwap(false, true) {
		close(c.ch)
	}
}

// Clients returns the number of connected streams.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Handler serves the SSE endpoint.
//
// Note the deliberate absence of a write deadline on this route: the connection
// is long-lived by design, and the heartbeat is what detects a dead peer.
func (h *Hub) Handler(heartbeat time.Duration) http.HandlerFunc {
	if heartbeat <= 0 {
		heartbeat = 20 * time.Second
	}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			Fail(w, r, ErrInternal.WithDetail("streaming is not supported by this server"))
			return
		}
		var topics []string
		if t := r.URL.Query().Get("topics"); t != "" {
			topics = strings.Split(t, ",")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		// Disable proxy buffering; without it nginx holds events until its
		// buffer fills, which looks exactly like a broken stream.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		lastID := uint64(0)
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			lastID, _ = strconv.ParseUint(v, 10, 64)
		}
		if v := r.URL.Query().Get("last_event_id"); v != "" && lastID == 0 {
			lastID, _ = strconv.ParseUint(v, 10, 64)
		}

		c := h.add(topics)
		defer h.remove(c)

		writeEvent := func(name string, ev StreamEvent) bool {
			payload, err := json.Marshal(ev.Data)
			if err != nil {
				return true
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, name, payload); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		// Resume.
		replay, needResync := h.replayFrom(lastID)
		if needResync {
			_, _ = fmt.Fprintf(w, "event: resync\ndata: {\"reason\":\"the replay buffer no longer covers your last event id; refetch state over REST\"}\n\n")
			flusher.Flush()
		}
		for _, ev := range replay {
			if !writeEvent(ev.Topic, ev) {
				return
			}
		}

		// Tell the client how long to wait before reconnecting.
		_, _ = fmt.Fprintf(w, "retry: 3000\n\n")
		flusher.Flush()

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, open := <-c.ch:
				if !open {
					_, _ = fmt.Fprintf(w, "event: overflow\ndata: {\"reason\":\"this connection could not keep up and was closed; reconnect to resume\"}\n\n")
					flusher.Flush()
					return
				}
				if !writeEvent(ev.Topic, ev) {
					return
				}
			case <-ticker.C:
				// A comment frame is the standard SSE keep-alive.
				if _, err := fmt.Fprintf(w, ": heartbeat %d\n\n", h.clock.Now().Unix()); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// Stream topic names. These are the dashboard's subscription vocabulary.
const (
	StreamQuotes      = "quote"
	StreamRegime      = "regime"
	StreamSignal      = "signal"
	StreamInvalidated = "signal_invalidated"
	StreamAlert       = "alert"
	StreamPrediction  = "prediction"
	StreamPortfolio   = "portfolio"
	StreamHealth      = "health"
)
