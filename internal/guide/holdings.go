// Package guide is the investor-facing layer of QuantOS: it reads a real (or
// model) book of holdings, marks it against the same history the research
// tools use, and reports what the data supports about it — exposure,
// concentration, correlation, drawdown, forecast volatility, and the
// arithmetic of a savings goal.
//
// It gives no advice. Every number it prints is a measurement with a window
// and a method, every forecast carries its own out-of-sample error, and the
// governance rules that bind the rest of the platform (docs/security/
// governance.md) bind this package too: no statement that an instrument
// *will* do anything, and the disclaimer on every record.
package guide

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// CashTicker is the reserved symbol for uninvested cash in a holdings file.
const CashTicker domain.Ticker = "CASH"

// Holding is one line of a holdings file.
type Holding struct {
	Ticker    domain.Ticker `json:"ticker"`
	Quantity  float64       `json:"quantity"`
	CostBasis float64       `json:"cost_basis,omitempty"` // per share, optional
	Note      string        `json:"note,omitempty"`
}

// Holdings is a book as the owner describes it: quantities, not weights,
// because weights are a function of prices and prices are the platform's
// job to supply.
type Holdings struct {
	Name     string    `json:"name"`
	Cash     float64   `json:"cash"`
	Lines    []Holding `json:"lines"`
	Source   string    `json:"source"`
	Currency string    `json:"currency"`
}

// LoadHoldings reads a CSV with a header row and the columns
//
//	ticker,quantity[,cost_basis][,note]
//
// A row whose ticker is CASH carries uninvested cash in the quantity column.
// Tickers are normalised the way the rest of the platform normalises them.
func LoadHoldings(path string) (Holdings, error) {
	f, err := os.Open(path)
	if err != nil {
		return Holdings{}, fmt.Errorf("guide: holdings %s: %w", path, err)
	}
	defer f.Close()
	h, err := ReadHoldings(f)
	if err != nil {
		return Holdings{}, fmt.Errorf("guide: holdings %s: %w", path, err)
	}
	h.Source = path
	if h.Name == "" {
		h.Name = strings.TrimSuffix(strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".csv"), ".CSV")
	}
	return h, nil
}

// ReadHoldings parses the holdings CSV format.
func ReadHoldings(r io.Reader) (Holdings, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return Holdings{}, fmt.Errorf("header: %w", err)
	}
	idx := map[string]int{}
	for i, c := range header {
		idx[strings.ToLower(strings.TrimSpace(c))] = i
	}
	col := func(names ...string) (int, bool) {
		for _, n := range names {
			if i, ok := idx[n]; ok {
				return i, true
			}
		}
		return 0, false
	}
	iT, okT := col("ticker", "symbol")
	iQ, okQ := col("quantity", "qty", "shares")
	if !okT || !okQ {
		return Holdings{}, fmt.Errorf("need ticker and quantity columns, got %q", header)
	}
	iC, hasC := col("cost_basis", "cost", "avg_price", "average_cost")
	iN, hasN := col("note", "notes", "comment")

	h := Holdings{Currency: "USD"}
	seen := map[domain.Ticker]int{}
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return Holdings{}, fmt.Errorf("line %d: %w", line, err)
		}
		if iT >= len(rec) || iQ >= len(rec) {
			continue
		}
		t := domain.NormalizeTicker(rec[iT])
		if t == "" {
			continue
		}
		q, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(rec[iQ]), ",", ""), 64)
		if err != nil {
			return Holdings{}, fmt.Errorf("line %d: quantity %q: %w", line, rec[iQ], err)
		}
		if t == CashTicker {
			h.Cash += q
			continue
		}
		if q == 0 {
			continue
		}
		hl := Holding{Ticker: t, Quantity: q}
		if hasC && iC < len(rec) && strings.TrimSpace(rec[iC]) != "" {
			hl.CostBasis, _ = strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(rec[iC]), ",", ""), 64)
		}
		if hasN && iN < len(rec) {
			hl.Note = strings.TrimSpace(rec[iN])
		}
		if i, dup := seen[t]; dup {
			// Two lots of the same name: merge quantities, weight the basis.
			prev := h.Lines[i]
			total := prev.Quantity + hl.Quantity
			if total != 0 && prev.CostBasis > 0 && hl.CostBasis > 0 {
				prev.CostBasis = (prev.CostBasis*prev.Quantity + hl.CostBasis*hl.Quantity) / total
			}
			prev.Quantity = total
			h.Lines[i] = prev
			continue
		}
		seen[t] = len(h.Lines)
		h.Lines = append(h.Lines, hl)
	}
	if len(h.Lines) == 0 && h.Cash == 0 {
		return Holdings{}, fmt.Errorf("no holdings")
	}
	sort.Slice(h.Lines, func(i, j int) bool { return h.Lines[i].Ticker < h.Lines[j].Ticker })
	return h, nil
}

// Tickers returns the instruments held, sorted.
func (h Holdings) Tickers() []domain.Ticker {
	out := make([]domain.Ticker, 0, len(h.Lines))
	for _, l := range h.Lines {
		out = append(out, l.Ticker)
	}
	return out
}
