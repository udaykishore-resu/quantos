// Package universe manages the configurable set of instruments QuantOS
// analyses, including the point-in-time membership that backtests need in order
// to avoid survivorship bias (ADR-007).
package universe

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Universe is a queryable, point-in-time set of instruments.
type Universe struct {
	mu       sync.RWMutex
	byTicker map[domain.Ticker]domain.Stock
	order    []domain.Ticker
	lists    map[string][]domain.Ticker // named lists: sp500, nasdaq100, watchlist, sectors
}

// New returns an empty universe.
func New() *Universe {
	return &Universe{
		byTicker: map[domain.Ticker]domain.Stock{},
		lists:    map[string][]domain.Ticker{},
	}
}

// Add inserts or replaces an instrument.
func (u *Universe) Add(s domain.Stock) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, exists := u.byTicker[s.Ticker]; !exists {
		u.order = append(u.order, s.Ticker)
		sort.Slice(u.order, func(i, j int) bool { return u.order[i] < u.order[j] })
	}
	u.byTicker[s.Ticker] = s
}

// Get returns an instrument.
func (u *Universe) Get(t domain.Ticker) (domain.Stock, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	s, ok := u.byTicker[t]
	return s, ok
}

// All returns every instrument in deterministic order.
func (u *Universe) All() []domain.Stock {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]domain.Stock, 0, len(u.order))
	for _, t := range u.order {
		out = append(out, u.byTicker[t])
	}
	return out
}

// Tickers returns every symbol in deterministic order.
func (u *Universe) Tickers() []domain.Ticker {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return append([]domain.Ticker(nil), u.order...)
}

// AsOf returns the instruments listed on the given date. This is the
// survivorship-bias guard: a backtest asking for 2019 gets the 2019 membership,
// including names that have since been delisted.
func (u *Universe) AsOf(at time.Time) []domain.Stock {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]domain.Stock, 0, len(u.order))
	for _, t := range u.order {
		s := u.byTicker[t]
		if s.Active(at) {
			out = append(out, s)
		}
	}
	return out
}

// List returns a named list, resolving to instruments present in the universe.
func (u *Universe) List(name string) []domain.Stock {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := []domain.Stock{}
	for _, t := range u.lists[strings.ToLower(name)] {
		if s, ok := u.byTicker[t]; ok {
			out = append(out, s)
		}
	}
	return out
}

// SetList defines a named list.
func (u *Universe) SetList(name string, tickers []domain.Ticker) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lists[strings.ToLower(name)] = append([]domain.Ticker(nil), tickers...)
}

// ListNames returns the defined list names.
func (u *Universe) ListNames() []string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]string, 0, len(u.lists))
	for k := range u.lists {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Sector returns every instrument in a sector.
func (u *Universe) Sector(sector string) []domain.Stock {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := []domain.Stock{}
	for _, t := range u.order {
		if strings.EqualFold(u.byTicker[t].Sector, sector) {
			out = append(out, u.byTicker[t])
		}
	}
	return out
}

// Sectors returns the distinct sectors present.
func (u *Universe) Sectors() []string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	seen := map[string]bool{}
	for _, t := range u.order {
		if s := u.byTicker[t].Sector; s != "" {
			seen[s] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Filter returns instruments matching a predicate, in deterministic order.
func (u *Universe) Filter(pred func(domain.Stock) bool) []domain.Stock {
	out := []domain.Stock{}
	for _, s := range u.All() {
		if pred(s) {
			out = append(out, s)
		}
	}
	return out
}

// Len returns the number of instruments.
func (u *Universe) Len() int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.order)
}

// fileFormat is the on-disk universe representation.
type fileFormat struct {
	Stocks []domain.Stock      `json:"stocks"`
	Lists  map[string][]string `json:"lists,omitempty"`
}

// LoadFile reads a universe from JSON.
func LoadFile(path string) (*Universe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("universe: read %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("universe: parse %s: %w", path, err)
	}
	u := New()
	for _, s := range f.Stocks {
		s.Ticker = domain.NormalizeTicker(string(s.Ticker))
		u.Add(s)
	}
	for name, list := range f.Lists {
		tt := make([]domain.Ticker, 0, len(list))
		for _, t := range list {
			tt = append(tt, domain.NormalizeTicker(t))
		}
		u.SetList(name, tt)
	}
	return u, nil
}

// Save writes the universe to JSON.
func (u *Universe) Save(path string) error {
	f := fileFormat{Stocks: u.All(), Lists: map[string][]string{}}
	for _, name := range u.ListNames() {
		for _, s := range u.List(name) {
			f.Lists[name] = append(f.Lists[name], string(s.Ticker))
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o640)
}

// Apply filters the universe by include/exclude lists and a size cap. Include
// entries may be tickers, list names or "sector:Technology".
func (u *Universe) Apply(include, exclude []string, max int) *Universe {
	out := New()
	for name := range u.lists {
		out.SetList(name, u.lists[name])
	}
	add := func(s domain.Stock) { out.Add(s) }

	if len(include) == 0 {
		for _, s := range u.All() {
			add(s)
		}
	} else {
		for _, inc := range include {
			switch {
			case strings.HasPrefix(strings.ToLower(inc), "sector:"):
				for _, s := range u.Sector(strings.TrimPrefix(inc, "sector:")) {
					add(s)
				}
			default:
				if list := u.List(inc); len(list) > 0 {
					for _, s := range list {
						add(s)
					}
					continue
				}
				if s, ok := u.Get(domain.NormalizeTicker(inc)); ok {
					add(s)
				}
			}
		}
	}
	for _, ex := range exclude {
		t := domain.NormalizeTicker(ex)
		out.mu.Lock()
		delete(out.byTicker, t)
		kept := out.order[:0]
		for _, x := range out.order {
			if x != t {
				kept = append(kept, x)
			}
		}
		out.order = kept
		out.mu.Unlock()
	}
	if max > 0 && out.Len() > max {
		// Keep the most liquid names, which is the defensible truncation for a
		// research platform: illiquid names are the ones risk would block anyway.
		all := out.All()
		sort.SliceStable(all, func(i, j int) bool { return all[i].AvgNotional > all[j].AvgNotional })
		trimmed := New()
		for name := range out.lists {
			trimmed.SetList(name, out.lists[name])
		}
		for _, s := range all[:max] {
			trimmed.Add(s)
		}
		return trimmed
	}
	return out
}
