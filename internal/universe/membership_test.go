package universe

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

const membershipCSV = `ticker,start,end,company,sector
AAPL,1982-11-30,,Apple,Technology
TSLA,2020-12-21,,Tesla,Consumer Discretionary
GE,1957-03-04,2018-06-26,General Electric,Industrials
GE,2023-01-01,,General Electric,Industrials
XOM,1957-03-04,,Exxon,Energy
`

func TestReadMembershipIntervals(t *testing.T) {
	m, err := ReadMembership(strings.NewReader(membershipCSV))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if m.Len() != 4 {
		t.Fatalf("expected 4 tickers, got %d", m.Len())
	}
	// Inclusion date is inclusive; the day before is not a member.
	if m.MemberAt("TSLA", day(2020, 12, 20)) {
		t.Error("TSLA should not be a member the day before inclusion")
	}
	if !m.MemberAt("TSLA", day(2020, 12, 21)) {
		t.Error("TSLA should be a member on its inclusion date")
	}
	// Removal date is exclusive.
	if !m.MemberAt("GE", day(2018, 6, 25)) || m.MemberAt("GE", day(2018, 6, 26)) {
		t.Error("GE removal boundary wrong")
	}
	// Re-inclusion is a second interval.
	if m.MemberAt("GE", day(2020, 1, 1)) || !m.MemberAt("GE", day(2024, 1, 1)) {
		t.Error("GE re-inclusion not honoured")
	}
	if len(m.Intervals("GE")) != 2 {
		t.Errorf("GE should have two intervals, got %v", m.Intervals("GE"))
	}
	// Unknown tickers are never members.
	if m.MemberAt("NVDA", day(2024, 1, 1)) {
		t.Error("a ticker absent from the table must not be a member")
	}
	got := m.MembersAt(day(2019, 1, 1))
	want := []domain.Ticker{"AAPL", "XOM"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("MembersAt(2019) = %v, want %v", got, want)
	}
}

func TestReadMembershipMergesOverlaps(t *testing.T) {
	m := NewMembership()
	m.Add("A", Interval{Start: day(2010, 1, 1), End: day(2012, 1, 1)})
	m.Add("A", Interval{Start: day(2011, 6, 1), End: day(2014, 1, 1)})
	m.Add("A", Interval{Start: day(2016, 1, 1)})
	m.Finalize()
	iv := m.Intervals("A")
	if len(iv) != 2 || !iv[0].End.Equal(day(2014, 1, 1)) || !iv[1].End.IsZero() {
		t.Errorf("merge produced %v", iv)
	}
}

func TestReadMembershipRejectsBadRows(t *testing.T) {
	if _, err := ReadMembership(strings.NewReader("ticker,start,end\nA,2020-01-01,2019-01-01\n")); err == nil {
		t.Error("end before start should be rejected")
	}
	if _, err := ReadMembership(strings.NewReader("symbol,from\nA,2020-01-01\n")); err == nil {
		t.Error("missing columns should be rejected")
	}
}

func TestUniverseConsultsMembership(t *testing.T) {
	u := Builtin()
	// Builtin fixtures are always active: no behaviour change without a table.
	if !u.Active("AAPL", day(2015, 1, 1)) {
		t.Fatal("builtin AAPL should be active without a membership table")
	}
	if u.Active("NOPE", day(2015, 1, 1)) {
		t.Fatal("unknown ticker should not be active")
	}

	m, err := ReadMembership(strings.NewReader(membershipCSV))
	if err != nil {
		t.Fatal(err)
	}
	u.SetMembership(m)

	if u.Active("TSLA", day(2019, 1, 1)) {
		t.Error("TSLA should be excluded before its inclusion date")
	}
	if !u.Active("TSLA", day(2021, 1, 1)) {
		t.Error("TSLA should be included after its inclusion date")
	}
	// A builtin name absent from the table is no longer a candidate: the table
	// is authoritative.
	if u.Active("MSFT", day(2021, 1, 1)) {
		t.Error("MSFT is not in the table and must not be active")
	}
	// A table name absent from builtin is added as a bare instrument.
	if s, ok := u.Get("GE"); !ok || s.Ticker != "GE" {
		t.Error("GE should have been added from the membership table")
	}
	asOf := u.AsOf(day(2019, 1, 1))
	for _, s := range asOf {
		if s.Ticker == "TSLA" || s.Ticker == "GE" {
			t.Errorf("AsOf(2019) should not include %s", s.Ticker)
		}
	}
	if len(asOf) != 2 {
		t.Errorf("AsOf(2019) = %d instruments, want 2 (AAPL, XOM)", len(asOf))
	}
}
