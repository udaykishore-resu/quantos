package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/udaykishoreresu/quantos/internal/auth"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/execution"
	"github.com/udaykishoreresu/quantos/internal/obs"
)

// paperVenue is a minimal in-memory paper venue.
type paperVenue struct {
	mu     sync.Mutex
	orders map[string]execution.Order
	held   []execution.Position
}

func (v *paperVenue) Name() string { return "test-paper" }
func (v *paperVenue) Paper() bool  { return true }
func (v *paperVenue) Account(context.Context) (execution.Account, error) {
	return execution.Account{Equity: 100_000, LastEquity: 100_000, BuyingPower: 50_000, Status: "ACTIVE", Paper: true}, nil
}
func (v *paperVenue) Positions(context.Context) ([]execution.Position, error) { return v.held, nil }
func (v *paperVenue) Submit(_ context.Context, r execution.OrderRequest) (execution.Order, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := execution.Order{ID: "o-" + r.ClientOrderID, ClientOrderID: r.ClientOrderID, Ticker: r.Ticker,
		Side: r.Side, Quantity: r.Quantity, LimitPrice: r.LimitPrice, Status: domain.OrderPending, VenueStatus: "accepted"}
	v.orders[r.ClientOrderID] = o
	return o, nil
}
func (v *paperVenue) OrderByClientID(_ context.Context, id string) (execution.Order, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.orders[id]
	if !ok {
		return execution.Order{}, execution.ErrOrderNotFound
	}
	return o, nil
}
func (v *paperVenue) CancelAll(context.Context) error { return nil }

type execHarness struct {
	srv   *httptest.Server
	venue *paperVenue
	authn *auth.Authenticator
}

func newExecHarness(t *testing.T) *execHarness {
	t.Helper()
	clock := obs.NewSimClock(time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC))
	authn, err := auth.New(auth.Config{
		Enabled: true, Issuer: "test", Audience: "test", HMACSecret: "0123456789abcdef0123456789abcdef",
		Users: []auth.User{
			{Subject: "uday", Password: "s3cret", Roles: []string{"operator"}},
			{Subject: "viewer", Password: "v", Roles: []string{"viewer"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	venue := &paperVenue{orders: map[string]execution.Order{}}
	quote := func(context.Context, domain.Ticker) (float64, error) { return 100, nil }
	lim := execution.DefaultLimits()
	lim.Allowlist = []domain.Ticker{"AAPL"}
	desk, err := execution.NewDesk(context.Background(), execution.DeskConfig{
		Venue: venue, Limits: lim, Quote: quote, Clock: clock,
		Health: func(context.Context) (bool, string) { return true, "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	prop, err := execution.NewProposer(execution.ProposerConfig{Desk: desk, Quote: quote, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Deps{Auth: authn, Desk: desk, Proposer: prop, Clock: clock})
	r := chi.NewRouter()
	a.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &execHarness{srv: srv, venue: venue, authn: authn}
}

func (h *execHarness) token(t *testing.T, user, pw string) string {
	t.Helper()
	tok, _, err := h.authn.Login(user, pw)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (h *execHarness) call(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func data(m map[string]any) map[string]any {
	d, _ := m["data"].(map[string]any)
	return d
}

func TestExecutionAPIProposeApprove(t *testing.T) {
	h := newExecHarness(t)
	tok := h.token(t, "uday", "s3cret")

	code, body := h.call(t, http.MethodPost, "/api/v1/execution/tickets", tok, map[string]any{
		"ticker": "aapl", "side": "buy", "quantity": 3, "rationale": "testing the desk",
	})
	if code != http.StatusCreated {
		t.Fatalf("propose: %d %v", code, body)
	}
	tk := data(body)
	id := tk["id"].(string)
	if tk["limit_price"].(float64) != 100.10 || tk["reference_price"].(float64) != 100 || tk["source"] != "manual" {
		t.Fatalf("manual proposal should be priced from the venue quote: %v", tk)
	}

	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/tickets/"+id+"/approve", tok,
		map[string]any{"password": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("a wrong password must not approve, got %d", code)
	}
	if len(h.venue.orders) != 0 {
		t.Fatal("a failed step-up reached the venue")
	}

	code, body = h.call(t, http.MethodPost, "/api/v1/execution/tickets/"+id+"/approve", tok,
		map[string]any{"password": "s3cret"})
	if code != http.StatusOK || data(body)["status"] != "SUBMITTED" || data(body)["decided_by"] != "uday" {
		t.Fatalf("approve: %d %v", code, body)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/tickets/"+id+"/approve", tok,
		map[string]any{"password": "s3cret"}); code != http.StatusConflict {
		t.Fatalf("a second approval must conflict, got %d", code)
	}

	code, body = h.call(t, http.MethodGet, "/api/v1/execution/tickets/"+id, tok, nil)
	if code != http.StatusOK || len(data(body)["history"].([]any)) != 3 {
		t.Fatalf("get: %d %v", code, body)
	}
}

func TestExecutionAPIGuardrailAndScopes(t *testing.T) {
	h := newExecHarness(t)
	tok := h.token(t, "uday", "s3cret")

	_, body := h.call(t, http.MethodPost, "/api/v1/execution/tickets", tok, map[string]any{
		"ticker": "AAPL", "side": "BUY", "quantity": 50, "rationale": "too big",
	})
	id := data(body)["id"].(string)
	code, body := h.call(t, http.MethodPost, "/api/v1/execution/tickets/"+id+"/approve", tok,
		map[string]any{"password": "s3cret"})
	if code != http.StatusConflict || data(body)["status"] != "BLOCKED" {
		t.Fatalf("an oversized order must come back BLOCKED with 409: %d %v", code, body)
	}

	viewer := h.token(t, "viewer", "v")
	if code, _ := h.call(t, http.MethodGet, "/api/v1/execution/tickets", viewer, nil); code != http.StatusOK {
		t.Fatalf("a viewer may read tickets, got %d", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/tickets", viewer, map[string]any{
		"ticker": "AAPL", "side": "BUY", "quantity": 1, "rationale": "x",
	}); code != http.StatusForbidden {
		t.Fatalf("a viewer must not propose, got %d", code)
	}
}

func TestExecutionAPIKillSwitch(t *testing.T) {
	h := newExecHarness(t)
	tok := h.token(t, "uday", "s3cret")
	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/halt", tok, nil); code != http.StatusOK {
		t.Fatalf("halt with no body must still halt, got %d", code)
	}
	code, body := h.call(t, http.MethodGet, "/api/v1/execution/status", tok, nil)
	if code != http.StatusOK || !data(body)["halt"].(map[string]any)["halted"].(bool) {
		t.Fatalf("status should report the halt: %d %v", code, body)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/tickets", tok, map[string]any{
		"ticker": "AAPL", "side": "BUY", "quantity": 1, "rationale": "x",
	}); code != http.StatusConflict {
		t.Fatalf("proposals must be refused while halted, got %d", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/resume", tok, map[string]any{"password": "nope"}); code != http.StatusUnauthorized {
		t.Fatalf("resume needs the password, got %d", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/v1/execution/resume", tok, map[string]any{"password": "s3cret"}); code != http.StatusOK {
		t.Fatalf("resume, got %d", code)
	}
}

func TestExecutionAPIWithoutADesk(t *testing.T) {
	authn, _ := auth.New(auth.Config{Enabled: true, HMACSecret: "0123456789abcdef0123456789abcdef",
		Users: []auth.User{{Subject: "uday", Password: "p", Roles: []string{"operator"}}}})
	r := chi.NewRouter()
	New(Deps{Auth: authn}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	tok, _, _ := authn.Login("uday", "p")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/execution/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when execution is disabled, got %d", resp.StatusCode)
	}
}
