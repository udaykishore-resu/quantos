package guide

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func flatSeries(t domain.Ticker, px float64, n int) []domain.Candle {
	out := make([]domain.Candle, n)
	d := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := range out {
		p := px * (1 + 0.001*float64(i%7))
		out[i] = domain.Candle{Ticker: t, Start: d, End: d.Add(24 * time.Hour), Close: p, Open: p, High: p, Low: p}
		d = d.AddDate(0, 0, 1)
	}
	return out
}

// The case that prompted look-through: half S&P 500 fund, a third one stock.
func TestIndexFundIsNotASingleName(t *testing.T) {
	series := Series{
		"SPY":  flatSeries("SPY", 100, 400),
		"VOO":  flatSeries("VOO", 100, 400),
		"AAPL": flatSeries("AAPL", 100, 400),
	}
	h := Holdings{Name: "t", Cash: 11, Lines: []Holding{{Ticker: "VOO", Quantity: 0.535}, {Ticker: "AAPL", Quantity: 0.355}}}
	for i := range h.Lines {
		h.Lines[i].Quantity *= 100
	}
	b, err := Assess(h, series, BookConfig{Caps: PersonalCaps()})
	if err != nil {
		t.Fatal(err)
	}
	var voo Position
	for _, p := range b.Positions {
		if p.Ticker == "VOO" {
			voo = p
		}
	}
	if voo.AssetClass != domain.AssetETF || voo.Sector != "Broad Market" {
		t.Fatalf("VOO must be recognised as a broad index fund: %+v", voo)
	}
	for _, f := range b.Flags {
		if strings.HasPrefix(f.Detail, "VOO") || strings.Contains(f.Detail, "Unknown") {
			t.Errorf("an index fund must not be flagged as a single name: %+v", f)
		}
	}
	if len(b.Exposures) == 0 || b.Exposures[0].Ticker != "AAPL" {
		t.Fatalf("AAPL should be the largest exposure: %+v", b.Exposures)
	}
	a := b.Exposures[0]
	wantVia := a.Direct / 0.355 * 0.535 * 0.06 // VOO weight × AAPL's weight in the S&P 500
	if a.ViaFunds < wantVia*0.99 || a.ViaFunds > wantVia*1.01 || a.Total <= a.Direct || len(a.Funds) != 1 || a.Funds[0] != "VOO" {
		t.Fatalf("AAPL look-through: %+v (want via ≈ %.4f)", a, wantVia)
	}
	var conc *Flag
	for i, f := range b.Flags {
		if f.Check == "concentration" {
			conc = &b.Flags[i]
		}
	}
	if conc == nil || conc.Level != "FAIL" || !strings.Contains(conc.Detail, "through VOO") || !strings.Contains(conc.Detail, "single-company cap is 25%") {
		t.Fatalf("want a FAIL naming the fund route and the personal cap, got %+v", conc)
	}
	// NVDA is held only through VOO, at about 4% of the book: no flag.
	for _, f := range b.Flags {
		if strings.HasPrefix(f.Detail, "NVDA") {
			t.Errorf("a company held only through an index fund at index weight must not be flagged: %+v", f)
		}
	}
}

func TestKnownFunds(t *testing.T) {
	if _, ok := KnownFund("AAPL"); ok {
		t.Fatal("a stock is not a fund")
	}
	for _, f := range []domain.Ticker{"VOO", "VTI", "QQQ", "BND", "VXUS"} {
		s, ok := KnownFund(f)
		if !ok || s.AssetClass != domain.AssetETF {
			t.Errorf("%s: %+v", f, s)
		}
	}
	if FundHoldings("BND") != nil {
		t.Error("a bond fund holds no single company worth looking through")
	}
}
