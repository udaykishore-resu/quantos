package guide

import (
	"bytes"
	"strings"
	"testing"
)

// A trimmed account-activity report in Robinhood's layout: newest first, money
// as "$1,234.56" or "($12.00)", and a free-text disclaimer after the rows.
const rhActivity = `"Activity Date","Process Date","Settle Date","Instrument","Description","Trans Code","Quantity","Price","Amount"
"10/9/2026","10/9/2026","10/10/2026","AAPL","Apple
CUSIP: 037833100","Sell","5","$336.00","$1,680.00"
"10/1/2026","10/1/2026","10/1/2026","","ACH Deposit","ACH","","","$2,000.00"
"9/15/2026","9/15/2026","9/15/2026","AAPL","Cash Div: R/D 2026-09-10","CDIV","","","$4.00"
"8/20/2026","8/20/2026","8/21/2026","NVDA","NVIDIA","Buy","10","$120.00","($1,200.00)"
"8/1/2026","8/1/2026","8/1/2026","VTI","Vanguard Total Stock Market ETF","ACATI","12","",""
"6/3/2026","6/3/2026","6/4/2026","AAPL","Apple","Buy","10","$300.00","($3,000.00)"
"5/2/2026","5/2/2026","5/5/2026","AAPL","Apple","Buy","10","$200.00","($2,000.00)"
"4/1/2026","4/1/2026","4/2/2026","TSLA","Tesla","Buy","2","$250.00","($500.00)"
"4/10/2026","4/10/2026","4/11/2026","TSLA","Tesla","Sell","2","$260.00","$520.00"
"3/1/2026","3/1/2026","3/2/2026","MSFT","Microsoft","Sell","3","$400.00","$1,200.00"
""
"The data provided is for informational purposes only."
`

func TestRobinhoodImport(t *testing.T) {
	imp, err := ReadRobinhoodActivity(strings.NewReader(rhActivity), 2500)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Holding{}
	for _, l := range imp.Holdings.Lines {
		got[string(l.Ticker)] = l
	}
	// AAPL: bought 10 @200 and 10 @300 (avg 250), sold 5 → 15 @ 250.
	if a := got["AAPL"]; a.Quantity != 15 || a.CostBasis != 250 {
		t.Fatalf("AAPL %+v", a)
	}
	if n := got["NVDA"]; n.Quantity != 10 || n.CostBasis != 120 {
		t.Fatalf("NVDA %+v", n)
	}
	if _, ok := got["TSLA"]; ok || imp.Closed != 1 {
		t.Fatalf("TSLA was bought and fully sold: holdings %+v closed %d", got, imp.Closed)
	}
	// The transfer in is not guessed at: it is listed, not applied.
	if _, ok := got["VTI"]; ok {
		t.Fatal("an ACATS transfer must not be applied automatically")
	}
	if len(imp.Review) != 1 || imp.Review[0].Code != "ACATI" || imp.Review[0].Quantity != 12 {
		t.Fatalf("review %+v", imp.Review)
	}
	// MSFT was sold without a buy in the report: the history is incomplete.
	if len(imp.Negative) != 1 || imp.Negative[0] != "MSFT" {
		t.Fatalf("negative %+v", imp.Negative)
	}
	if imp.Applied != 7 || imp.Holdings.Cash != 2500 {
		t.Fatalf("applied %d cash %v", imp.Applied, imp.Holdings.Cash)
	}
	if imp.From.Format("2006-01-02") != "2026-03-01" || imp.To.Format("2006-01-02") != "2026-10-09" {
		t.Fatalf("range %s..%s", imp.From, imp.To)
	}

	// What is written must be what the book assessment reads.
	var buf bytes.Buffer
	if err := WriteHoldings(&buf, imp.Holdings); err != nil {
		t.Fatal(err)
	}
	back, err := ReadHoldings(&buf)
	if err != nil {
		t.Fatalf("round trip: %v\n%s", err, buf.String())
	}
	if back.Cash != 2500 || len(back.Lines) != 2 {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestRobinhoodRejectsOtherFiles(t *testing.T) {
	if _, err := ReadRobinhoodActivity(strings.NewReader("ticker,quantity\nAAPL,1\n"), 0); err == nil ||
		!strings.Contains(err.Error(), "account-activity report") {
		t.Fatalf("want a clear error for a non-Robinhood file, got %v", err)
	}
}

func TestParseNumber(t *testing.T) {
	for in, want := range map[string]float64{
		"$1,234.56": 1234.56, "($12.00)": -12, "0.5": 0.5, "10S": 10, "": 0, "abc": 0,
	} {
		if got := parseNumber(in); got != want {
			t.Errorf("%q → %v, want %v", in, got, want)
		}
	}
}
