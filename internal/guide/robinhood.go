package guide

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// Robinhood import.
//
// Robinhood does not export current positions. It exports an account-activity
// report (Account → Reports and statements → Account activity), a CSV of every
// transaction, from which positions have to be rebuilt by replaying the trades.
// That replay is only as good as the history it is given, so the importer is
// deliberately conservative:
//
//   - Buy and Sell rows are applied; their quantities define the position and
//     an average cost basis.
//   - Every other row that names an instrument and carries a quantity — a
//     split, a transfer in from another broker, a stock reward, a merger — is
//     NOT applied. It is listed for review, because guessing its sign is how a
//     position ends up wrong without anyone noticing.
//   - Cash is not derived: deposits, fees, interest and card activity make
//     that reconstruction unreliable. Pass it separately.
//
// The result is meant to be checked once against the Robinhood app and then
// edited by hand where it differs.

// RobinhoodImport is the outcome of replaying an activity report.
type RobinhoodImport struct {
	Holdings Holdings `json:"holdings"`
	// From and To are the dates of the first and last rows read.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Applied counts the Buy and Sell rows replayed.
	Applied int `json:"applied"`
	// Review lists rows that affect a position but were not applied.
	Review []RobinhoodRow `json:"review,omitempty"`
	// Negative lists tickers whose replayed quantity went below zero: the
	// report starts after the shares were acquired, or a transfer is missing.
	Negative []domain.Ticker `json:"negative,omitempty"`
	// Closed counts tickers bought and fully sold within the report.
	Closed int `json:"closed"`
}

// RobinhoodRow is one activity row that needs a person's judgement.
type RobinhoodRow struct {
	Date        time.Time     `json:"date"`
	Ticker      domain.Ticker `json:"ticker"`
	Code        string        `json:"code"`
	Description string        `json:"description"`
	Quantity    float64       `json:"quantity"`
}

// ReadRobinhoodActivity replays a Robinhood account-activity CSV into holdings.
func ReadRobinhoodActivity(r io.Reader, cash float64) (RobinhoodImport, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return RobinhoodImport{}, fmt.Errorf("robinhood: header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	need := []string{"activity date", "instrument", "trans code", "quantity"}
	for _, n := range need {
		if _, ok := idx[n]; !ok {
			return RobinhoodImport{}, fmt.Errorf("robinhood: column %q missing; this does not look like an account-activity report (header %q)", n, header)
		}
	}
	iDate, iInst, iCode, iQty := idx["activity date"], idx["instrument"], idx["trans code"], idx["quantity"]
	iPrice, hasPrice := idx["price"]
	iDesc, hasDesc := idx["description"]

	type row struct {
		date  time.Time
		t     domain.Ticker
		code  string
		qty   float64
		price float64
		desc  string
		order int
	}
	var rows []row
	line := 1
	for {
		rec, err := cr.Read()
		line++
		if err == io.EOF {
			break
		}
		if err != nil {
			return RobinhoodImport{}, fmt.Errorf("robinhood: line %d: %w", line, err)
		}
		get := func(i int) string {
			if i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		// The file ends with a free-text disclaimer; those lines have no date.
		d, err := time.Parse("1/2/2006", get(iDate))
		if err != nil {
			continue
		}
		rw := row{date: d, t: domain.NormalizeTicker(get(iInst)), code: get(iCode), order: line}
		rw.qty = parseNumber(get(iQty))
		if hasPrice {
			rw.price = parseNumber(get(iPrice))
		}
		if hasDesc {
			rw.desc = strings.Join(strings.Fields(get(iDesc)), " ")
		}
		rows = append(rows, rw)
	}
	if len(rows) == 0 {
		return RobinhoodImport{}, fmt.Errorf("robinhood: no dated rows")
	}
	// Reports list newest first; replay oldest first, and keep the file's
	// order for rows on the same day (reversed, since the file is reversed).
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].date.Equal(rows[j].date) {
			return rows[i].date.Before(rows[j].date)
		}
		return rows[i].order > rows[j].order
	})

	type pos struct{ qty, cost float64 }
	book := map[domain.Ticker]*pos{}
	out := RobinhoodImport{From: rows[0].date, To: rows[len(rows)-1].date}
	negative := map[domain.Ticker]bool{}
	seen := map[domain.Ticker]bool{}
	for _, rw := range rows {
		if rw.t == "" || rw.qty == 0 {
			continue
		}
		p := book[rw.t]
		if p == nil {
			p = &pos{}
			book[rw.t] = p
		}
		switch strings.ToLower(rw.code) {
		case "buy":
			seen[rw.t] = true
			p.cost += rw.qty * rw.price
			p.qty += rw.qty
			out.Applied++
		case "sell":
			seen[rw.t] = true
			if p.qty > 0 {
				// Average cost: a sale removes cost in proportion to shares.
				frac := math.Min(rw.qty/p.qty, 1)
				p.cost -= p.cost * frac
			}
			p.qty -= rw.qty
			if p.qty < -1e-9 {
				negative[rw.t] = true
			}
			out.Applied++
		default:
			out.Review = append(out.Review, RobinhoodRow{
				Date: rw.date, Ticker: rw.t, Code: rw.code, Description: rw.desc, Quantity: rw.qty,
			})
		}
	}

	h := Holdings{Name: "robinhood", Cash: cash, Currency: "USD",
		Source: fmt.Sprintf("Robinhood activity %s to %s", out.From.Format("2006-01-02"), out.To.Format("2006-01-02"))}
	for t, p := range book {
		if math.Abs(p.qty) < 1e-6 {
			if seen[t] {
				out.Closed++
			}
			continue
		}
		if p.qty < 0 {
			continue // reported in Negative; a short is not what happened
		}
		hd := Holding{Ticker: t, Quantity: roundQty(p.qty)}
		if p.cost > 0 {
			hd.CostBasis = math.Round(p.cost/p.qty*100) / 100
		}
		h.Lines = append(h.Lines, hd)
	}
	sort.Slice(h.Lines, func(i, j int) bool { return h.Lines[i].Ticker < h.Lines[j].Ticker })
	for t := range negative {
		out.Negative = append(out.Negative, t)
	}
	sort.Slice(out.Negative, func(i, j int) bool { return out.Negative[i] < out.Negative[j] })
	out.Holdings = h
	return out, nil
}

// parseNumber reads Robinhood's number formats: "$1,234.56", "($12.00)",
// "0.5", "10S" (a trailing letter on some quantities).
func parseNumber(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	neg := strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")")
	s = strings.Trim(s, "()")
	s = strings.NewReplacer("$", "", ",", "").Replace(s)
	s = strings.TrimRightFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' })
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	if neg {
		v = -v
	}
	return v
}

func roundQty(q float64) float64 { return math.Round(q*1e6) / 1e6 }

// WriteHoldings writes holdings in the format LoadHoldings reads.
func WriteHoldings(w io.Writer, h Holdings) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ticker", "quantity", "cost_basis", "note"})
	if h.Cash != 0 {
		_ = cw.Write([]string{string(CashTicker), strconv.FormatFloat(h.Cash, 'f', 2, 64), "", "cash"})
	}
	for _, l := range h.Lines {
		cb := ""
		if l.CostBasis > 0 {
			cb = strconv.FormatFloat(l.CostBasis, 'f', 2, 64)
		}
		_ = cw.Write([]string{string(l.Ticker), strconv.FormatFloat(l.Quantity, 'f', -1, 64), cb, l.Note})
	}
	cw.Flush()
	return cw.Error()
}
