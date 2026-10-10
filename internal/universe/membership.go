package universe

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Membership is a point-in-time record of index constituents: which tickers
// were members on which dates. It is the survivorship-bias guard made
// concrete.
//
// The builtin universe is a list of today's large caps, and every name on it
// is there *because* it did well. A backtest over that list is a backtest over
// the winners, and the 2–5% a year that flatters is not edge. With a
// membership table loaded, a bar for a ticker that was not a member on that
// date still warms the feature engine (the data existed) but can never become
// a candidate, a prediction or a signal.
//
// What this cannot fix: names that were delisted usually have no price history
// at free data sources, so they are absent from the replay directory rather
// than present and excluded. The table removes the pre-inclusion run-up bias;
// it cannot resurrect the dead. The report says so.
type Membership struct {
	intervals map[domain.Ticker][]Interval
	source    string
}

// Interval is one span of membership. A zero End means "still a member".
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitempty"`
}

// Contains reports whether the interval covers the instant. Start is
// inclusive and End exclusive, which matches how index providers announce
// changes: "effective prior to the open on <date>".
func (iv Interval) Contains(at time.Time) bool {
	if at.Before(iv.Start) {
		return false
	}
	if !iv.End.IsZero() && !at.Before(iv.End) {
		return false
	}
	return true
}

// NewMembership returns an empty table.
func NewMembership() *Membership {
	return &Membership{intervals: map[domain.Ticker][]Interval{}}
}

// Add records an interval. Overlapping intervals for the same ticker are
// merged on Finalize.
func (m *Membership) Add(t domain.Ticker, iv Interval) {
	t = domain.NormalizeTicker(string(t))
	m.intervals[t] = append(m.intervals[t], iv)
}

// Finalize sorts and merges each ticker's intervals.
func (m *Membership) Finalize() {
	for t, list := range m.intervals {
		sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
		merged := list[:0]
		for _, iv := range list {
			n := len(merged)
			if n > 0 {
				prev := &merged[n-1]
				if prev.End.IsZero() || !iv.Start.After(prev.End) {
					// Overlap or touch: extend.
					if iv.End.IsZero() || (!prev.End.IsZero() && iv.End.After(prev.End)) {
						prev.End = iv.End
					}
					continue
				}
			}
			merged = append(merged, iv)
		}
		m.intervals[t] = merged
	}
}

// MemberAt reports whether the ticker was a member at the instant. A ticker
// absent from the table is not a member: the table is authoritative for the
// universe it describes.
func (m *Membership) MemberAt(t domain.Ticker, at time.Time) bool {
	for _, iv := range m.intervals[t] {
		if iv.Contains(at) {
			return true
		}
	}
	return false
}

// Intervals returns the recorded spans for a ticker.
func (m *Membership) Intervals(t domain.Ticker) []Interval {
	return append([]Interval(nil), m.intervals[t]...)
}

// Tickers returns every ticker that was ever a member, sorted.
func (m *Membership) Tickers() []domain.Ticker {
	out := make([]domain.Ticker, 0, len(m.intervals))
	for t := range m.intervals {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MembersAt returns the members on a date, sorted.
func (m *Membership) MembersAt(at time.Time) []domain.Ticker {
	var out []domain.Ticker
	for t, list := range m.intervals {
		for _, iv := range list {
			if iv.Contains(at) {
				out = append(out, t)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Len returns the number of tickers with at least one interval.
func (m *Membership) Len() int { return len(m.intervals) }

// Source names where the table came from, for provenance.
func (m *Membership) Source() string { return m.source }

// LoadMembership reads a CSV with a header row and the columns
//
//	ticker,start,end[,company,sector]
//
// Dates are YYYY-MM-DD. An empty end means current membership. Extra columns
// are ignored, so the file produced by scripts/build_membership.py and a
// hand-maintained one are both accepted.
func LoadMembership(path string) (*Membership, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("universe: membership %s: %w", path, err)
	}
	defer f.Close()
	m, err := ReadMembership(f)
	if err != nil {
		return nil, fmt.Errorf("universe: membership %s: %w", path, err)
	}
	m.source = path
	return m, nil
}

// ReadMembership parses the membership CSV format.
func ReadMembership(r io.Reader) (*Membership, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"ticker", "start"} {
		if _, ok := idx[need]; !ok {
			return nil, fmt.Errorf("missing column %q", need)
		}
	}
	m := NewMembership()
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		ticker := strings.TrimSpace(rec[idx["ticker"]])
		if ticker == "" {
			continue
		}
		start, err := parseDay(rec[idx["start"]])
		if err != nil {
			return nil, fmt.Errorf("line %d: start: %w", line, err)
		}
		var end time.Time
		if i, ok := idx["end"]; ok && i < len(rec) && strings.TrimSpace(rec[i]) != "" {
			end, err = parseDay(rec[i])
			if err != nil {
				return nil, fmt.Errorf("line %d: end: %w", line, err)
			}
			if !end.After(start) {
				return nil, fmt.Errorf("line %d: end %s is not after start %s", line, end.Format("2006-01-02"), start.Format("2006-01-02"))
			}
		}
		m.Add(domain.Ticker(ticker), Interval{Start: start, End: end})
	}
	m.Finalize()
	if m.Len() == 0 {
		return nil, fmt.Errorf("no rows")
	}
	return m, nil
}

func parseDay(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006/01/02", "01/02/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			y, mo, d := t.UTC().Date()
			return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", s)
}

// --- Universe integration ----------------------------------------------------

// SetMembership attaches a point-in-time table. Once set, Active and AsOf
// consult it instead of the per-stock ListedAt/DelistedAt fields, and tickers
// in the table but not in the universe are added as bare instruments so a
// replay directory that covers them is usable.
func (u *Universe) SetMembership(m *Membership) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.membership = m
	if m == nil {
		return
	}
	for _, t := range m.Tickers() {
		if _, ok := u.byTicker[t]; ok {
			continue
		}
		u.order = append(u.order, t)
		u.byTicker[t] = domain.Stock{
			Ticker: t, Currency: "USD", AssetClass: domain.AssetEquity,
			Tags: []string{"membership"},
		}
	}
	sort.Slice(u.order, func(i, j int) bool { return u.order[i] < u.order[j] })
}

// Membership returns the attached table, or nil.
func (u *Universe) Membership() *Membership {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.membership
}

// Active reports whether the ticker is a tradable candidate at the instant.
// With a membership table it is the table's answer; otherwise it is the
// stock's own listing dates. Macro and sector-context symbols are never in a
// membership table and are handled by the caller, which does not trade them.
func (u *Universe) Active(t domain.Ticker, at time.Time) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.membership != nil {
		return u.membership.MemberAt(t, at)
	}
	s, ok := u.byTicker[t]
	if !ok {
		return false
	}
	return s.Active(at)
}
