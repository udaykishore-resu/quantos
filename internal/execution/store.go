package execution

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Store persists tickets and the kill-switch state. The desk keeps a working
// copy in memory; the store is what survives a restart.
type Store interface {
	// InsertTicket records a new ticket. It returns ErrDuplicateSignal when a
	// ticket for the same signal already exists.
	InsertTicket(ctx context.Context, t Ticket) error
	// UpdateTicket writes t only if the stored ticket is still in status from.
	// It returns ErrConflict otherwise, which is how two replicas (or two
	// clicks) are prevented from both acting on one ticket.
	UpdateTicket(ctx context.Context, t Ticket, from TicketStatus) error
	// ListTickets returns the most recent tickets, newest first.
	ListTickets(ctx context.Context, limit int) ([]Ticket, error)
	SaveHalt(ctx context.Context, h Halt) error
	LoadHalt(ctx context.Context) (Halt, error)
}

// Halt is the persisted kill-switch state. It is persisted so that a restart
// can never silently resume trading.
type Halt struct {
	Halted bool      `json:"halted"`
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at,omitempty"`
}

// Store errors.
var (
	ErrConflict        = errors.New("execution: ticket changed underneath this update")
	ErrDuplicateSignal = errors.New("execution: a ticket for this signal already exists")
)

// MemoryStore is a Store for the embedded mode and tests. It does not survive
// a restart; the desk logs that loudly when it is used outside a test.
type MemoryStore struct {
	mu      sync.Mutex
	tickets map[string]Ticket
	signals map[string]string
	halt    Halt
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tickets: map[string]Ticket{}, signals: map[string]string{}}
}

// InsertTicket implements Store.
func (m *MemoryStore) InsertTicket(_ context.Context, t Ticket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tickets[t.ID]; ok {
		return ErrConflict
	}
	if t.SignalID != "" {
		if _, ok := m.signals[t.SignalID]; ok {
			return ErrDuplicateSignal
		}
		m.signals[t.SignalID] = t.ID
	}
	m.tickets[t.ID] = t.clone()
	return nil
}

// UpdateTicket implements Store.
func (m *MemoryStore) UpdateTicket(_ context.Context, t Ticket, from TicketStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.tickets[t.ID]
	if !ok || cur.Status != from {
		return ErrConflict
	}
	m.tickets[t.ID] = t.clone()
	return nil
}

// ListTickets implements Store.
func (m *MemoryStore) ListTickets(_ context.Context, limit int) ([]Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Ticket, 0, len(m.tickets))
	for _, t := range m.tickets {
		out = append(out, t.clone())
	}
	sortNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SaveHalt implements Store.
func (m *MemoryStore) SaveHalt(_ context.Context, h Halt) error {
	m.mu.Lock()
	m.halt = h
	m.mu.Unlock()
	return nil
}

// LoadHalt implements Store.
func (m *MemoryStore) LoadHalt(context.Context) (Halt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.halt, nil
}

func sortNewestFirst(ts []Ticket) {
	sort.Slice(ts, func(i, j int) bool {
		if !ts[i].CreatedAt.Equal(ts[j].CreatedAt) {
			return ts[i].CreatedAt.After(ts[j].CreatedAt)
		}
		return ts[i].ID > ts[j].ID
	})
}
