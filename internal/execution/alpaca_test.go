package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

func TestNewAlpacaPaperRefusesLiveAndForeignHosts(t *testing.T) {
	for _, u := range []string{"https://api.alpaca.markets", "https://evil.example.com", "http://paper-api.alpaca.markets"} {
		if _, err := NewAlpacaPaper(AlpacaConfig{KeyID: "k", SecretKey: "s", BaseURL: u}); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
	if _, err := NewAlpacaPaper(AlpacaConfig{KeyID: "k", SecretKey: "s"}); err != nil {
		t.Fatalf("the paper default must be accepted: %v", err)
	}
	if _, err := NewAlpacaPaper(AlpacaConfig{}); err == nil {
		t.Fatal("missing keys must be refused")
	}
}

func alpacaServer(t *testing.T) (*Alpaca, *[]map[string]any) {
	t.Helper()
	var posted []map[string]any
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("APCA-API-KEY-ID") != "k" || r.Header.Get("APCA-API-SECRET-KEY") != "s" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":40110000,"message":"request is not authorized"}`))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v2/account", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			_, _ = w.Write([]byte(`{"equity":"101000.50","last_equity":"100000","cash":"20000","buying_power":"40000","currency":"USD","status":"ACTIVE","trading_blocked":false}`))
		}
	})
	mux.HandleFunc("GET /v2/positions", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			_, _ = w.Write([]byte(`[{"symbol":"AAPL","qty":"10","side":"long","avg_entry_price":"180","current_price":"190","market_value":"1900","unrealized_pl":"100"},
			                       {"symbol":"XYZ","qty":"3","side":"short","avg_entry_price":"50","current_price":"40","market_value":"-120","unrealized_pl":"30"}]`))
		}
	})
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body)
		if len(posted) > 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":40010001,"message":"client_order_id must be unique"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"o-1","client_order_id":"qos-1","symbol":"AAPL","qty":"2","side":"buy","limit_price":"190.5","status":"accepted","filled_qty":"0","submitted_at":"2026-10-12T14:30:00Z"}`))
	})
	mux.HandleFunc("GET /v2/orders:by_client_order_id", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_order_id") != "qos-1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":40410000,"message":"order not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"o-1","client_order_id":"qos-1","symbol":"AAPL","qty":"2","side":"buy","limit_price":"190.5","status":"filled","filled_qty":"2","filled_avg_price":"190.4"}`))
	})
	mux.HandleFunc("DELETE /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("GET /v2/stocks/{sym}/quotes/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("feed") != "iex" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.PathValue("sym") {
		case "AAPL":
			_, _ = w.Write([]byte(`{"symbol":"AAPL","quote":{"ap":190.52,"bp":190.48}}`))
		default: // a one-sided or absurdly wide book
			_, _ = w.Write([]byte(`{"symbol":"THIN","quote":{"ap":12.0,"bp":0}}`))
		}
	})
	mux.HandleFunc("GET /v2/stocks/{sym}/trades/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"symbol":"THIN","trade":{"p":11.37}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a, err := NewAlpacaPaper(AlpacaConfig{KeyID: "k", SecretKey: "s", BaseURL: srv.URL, DataURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return a, &posted
}

func TestAlpacaAccountAndPositions(t *testing.T) {
	a, _ := alpacaServer(t)
	ctx := context.Background()
	acct, err := a.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Equity != 101000.5 || acct.Blocked || !acct.Paper {
		t.Fatalf("account %+v", acct)
	}
	if got := acct.DayPnLPct(); got < 0.01 || got > 0.0101 {
		t.Fatalf("day pnl %v", got)
	}
	pos, err := a.Positions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 2 || pos[0].Ticker != "AAPL" || pos[0].Quantity != 10 || pos[1].Quantity != -3 {
		t.Fatalf("positions %+v", pos)
	}
}

func TestAlpacaSubmitIsALimitOrderAndIdempotent(t *testing.T) {
	a, posted := alpacaServer(t)
	ctx := context.Background()
	req := OrderRequest{ClientOrderID: "qos-1", Ticker: "AAPL", Side: domain.OrderBuy, Quantity: 2, LimitPrice: 190.5}
	o, err := a.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != domain.OrderPending || o.VenueStatus != "accepted" {
		t.Fatalf("order %+v", o)
	}
	body := (*posted)[0]
	if body["type"] != "limit" || body["limit_price"] != "190.50" || body["qty"] != "2" ||
		body["side"] != "buy" || body["time_in_force"] != "day" || body["client_order_id"] != "qos-1" {
		t.Fatalf("request body %+v", body)
	}
	// A retry with the same client order id returns the existing order.
	o2, err := a.Submit(ctx, req)
	if err != nil {
		t.Fatalf("duplicate submit should resolve to the existing order: %v", err)
	}
	if o2.Status != domain.OrderFilled || o2.AvgFillPrice != 190.4 {
		t.Fatalf("existing order %+v", o2)
	}
}

func TestAlpacaErrors(t *testing.T) {
	a, _ := alpacaServer(t)
	ctx := context.Background()
	if _, err := a.OrderByClientID(ctx, "nope"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("want ErrOrderNotFound, got %v", err)
	}
	if err := a.CancelAll(ctx); err != nil {
		t.Fatalf("207 from cancel-all is success: %v", err)
	}
	bad, _ := NewAlpacaPaper(AlpacaConfig{KeyID: "wrong", SecretKey: "s", BaseURL: a.baseURL, DataURL: a.dataURL})
	_, err := bad.Account(ctx)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("want a 401 APIError, got %v", err)
	}
}

func TestAlpacaStatusMapping(t *testing.T) {
	cases := map[string]domain.OrderStatus{
		"new": domain.OrderPending, "accepted": domain.OrderPending, "partially_filled": domain.OrderPartial,
		"filled": domain.OrderFilled, "canceled": domain.OrderCancelled, "expired": domain.OrderCancelled,
		"rejected": domain.OrderRejected,
	}
	for in, want := range cases {
		if got := alpacaStatus(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

func TestAlpacaQuote(t *testing.T) {
	a, _ := alpacaServer(t)
	ctx := context.Background()
	if px, err := a.Quote(ctx, "AAPL"); err != nil || px != 190.5 {
		t.Fatalf("want the 190.50 midpoint, got %v %v", px, err)
	}
	if px, err := a.Quote(ctx, "THIN"); err != nil || px != 11.37 {
		t.Fatalf("a one-sided book should fall back to the last trade, got %v %v", px, err)
	}
	if _, err := NewAlpacaPaper(AlpacaConfig{KeyID: "k", SecretKey: "s", DataURL: "https://data.example.com"}); err == nil {
		t.Fatal("a foreign data host must be refused")
	}
}
