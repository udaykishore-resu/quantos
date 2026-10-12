package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// AlpacaPaperURL is the only Alpaca trading host this build will talk to.
const AlpacaPaperURL = "https://paper-api.alpaca.markets"

// AlpacaDataURL is Alpaca's market-data host. It is read-only: it serves
// quotes, not orders.
const AlpacaDataURL = "https://data.alpaca.markets"

// alpacaLiveHost is named so that the refusal below is explicit rather than a
// side effect of an allowlist.
const alpacaLiveHost = "api.alpaca.markets"

// AlpacaConfig parameterises the Alpaca adapter. Keys are read from the
// environment by the caller (QUANTOS_ALPACA_KEY_ID, QUANTOS_ALPACA_SECRET_KEY)
// and are never stored in configuration, logged, or sent to the frontend.
type AlpacaConfig struct {
	KeyID     string
	SecretKey string
	// BaseURL defaults to AlpacaPaperURL. Tests point it at a loopback server;
	// anything else is refused.
	BaseURL string
	// DataURL defaults to AlpacaDataURL; tests point it at a loopback server.
	DataURL string
	// Feed is the market-data feed: "iex" (free) or "sip" (paid).
	Feed    string
	Timeout time.Duration
}

// Alpaca is a Venue backed by Alpaca's trading API, written against the
// documented HTTP contract rather than a vendor SDK (as internal/llm is).
type Alpaca struct {
	keyID, secret string
	baseURL       string
	dataURL       string
	feed          string
	client        *http.Client
}

// NewAlpacaPaper builds an adapter for Alpaca's paper environment.
//
// It refuses Alpaca's live host and any host other than the paper API or a
// loopback test server. That refusal is the code half of governance rule G-8;
// removing it is a decision, not a setting.
func NewAlpacaPaper(cfg AlpacaConfig) (*Alpaca, error) {
	if cfg.KeyID == "" || cfg.SecretKey == "" {
		return nil, errors.New("execution: alpaca key id and secret are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = AlpacaPaperURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("execution: alpaca base url: %w", err)
	}
	host := u.Hostname()
	switch {
	case host == alpacaLiveHost:
		return nil, errors.New("execution: refusing Alpaca's live API; this build routes to paper only (governance rule G-8)")
	case u.Scheme == "https" && host == "paper-api.alpaca.markets":
	case isLoopback(host):
	default:
		return nil, fmt.Errorf("execution: refusing alpaca base url %q; only %s is allowed", cfg.BaseURL, AlpacaPaperURL)
	}
	if cfg.DataURL == "" {
		cfg.DataURL = AlpacaDataURL
	}
	du, err := url.Parse(cfg.DataURL)
	if err != nil {
		return nil, fmt.Errorf("execution: alpaca data url: %w", err)
	}
	if !(du.Scheme == "https" && du.Hostname() == "data.alpaca.markets") && !isLoopback(du.Hostname()) {
		return nil, fmt.Errorf("execution: refusing alpaca data url %q; only %s is allowed", cfg.DataURL, AlpacaDataURL)
	}
	if cfg.Feed == "" {
		cfg.Feed = "iex"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Alpaca{
		keyID: cfg.KeyID, secret: cfg.SecretKey,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		dataURL: strings.TrimRight(cfg.DataURL, "/"),
		feed:    cfg.Feed,
		client:  &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Name implements Venue.
func (a *Alpaca) Name() string { return "alpaca-paper" }

// Paper implements Venue. The constructor guarantees it.
func (a *Alpaca) Paper() bool { return true }

// APIError is a non-2xx response from Alpaca.
type APIError struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("alpaca: HTTP %d: %s (code %d)", e.Status, e.Message, e.Code)
}

func (a *Alpaca) do(ctx context.Context, method, path string, body, out any) error {
	return a.doAt(ctx, a.baseURL, method, path, body, out)
}

func (a *Alpaca) doAt(ctx context.Context, base, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("alpaca: marshal: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", a.keyID)
	req.Header.Set("APCA-API-SECRET-KEY", a.secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("alpaca: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("alpaca: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(raw, apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(raw))
		}
		return apiErr
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("alpaca: decode %s: %w", path, err)
		}
	}
	return nil
}

// Alpaca encodes every number as a string.
func num(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

type alpacaAccount struct {
	Equity         string `json:"equity"`
	LastEquity     string `json:"last_equity"`
	Cash           string `json:"cash"`
	BuyingPower    string `json:"buying_power"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
	TradingBlocked bool   `json:"trading_blocked"`
	AccountBlocked bool   `json:"account_blocked"`
}

// Account implements Venue.
func (a *Alpaca) Account(ctx context.Context) (Account, error) {
	var r alpacaAccount
	if err := a.do(ctx, http.MethodGet, "/v2/account", nil, &r); err != nil {
		return Account{}, err
	}
	return Account{
		Equity: num(r.Equity), LastEquity: num(r.LastEquity), Cash: num(r.Cash),
		BuyingPower: num(r.BuyingPower), Currency: r.Currency, Status: r.Status,
		Blocked: r.TradingBlocked || r.AccountBlocked || r.Status != "ACTIVE",
		Paper:   true,
	}, nil
}

type alpacaPosition struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty"`
	Side          string `json:"side"`
	AvgEntryPrice string `json:"avg_entry_price"`
	CurrentPrice  string `json:"current_price"`
	MarketValue   string `json:"market_value"`
	UnrealizedPL  string `json:"unrealized_pl"`
}

// Positions implements Venue.
func (a *Alpaca) Positions(ctx context.Context) ([]Position, error) {
	var rs []alpacaPosition
	if err := a.do(ctx, http.MethodGet, "/v2/positions", nil, &rs); err != nil {
		return nil, err
	}
	out := make([]Position, 0, len(rs))
	for _, r := range rs {
		qty := num(r.Qty)
		if r.Side == "short" && qty > 0 {
			qty = -qty
		}
		out = append(out, Position{
			Ticker: domain.NormalizeTicker(r.Symbol), Quantity: qty,
			AvgPrice: num(r.AvgEntryPrice), MarketPrice: num(r.CurrentPrice),
			MarketValue: num(r.MarketValue), UnrealizedPnL: num(r.UnrealizedPL),
		})
	}
	return out, nil
}

type alpacaOrderReq struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	TimeInForce   string `json:"time_in_force"`
	LimitPrice    string `json:"limit_price"`
	ClientOrderID string `json:"client_order_id"`
}

type alpacaOrder struct {
	ID             string    `json:"id"`
	ClientOrderID  string    `json:"client_order_id"`
	Symbol         string    `json:"symbol"`
	Qty            string    `json:"qty"`
	Side           string    `json:"side"`
	LimitPrice     string    `json:"limit_price"`
	Status         string    `json:"status"`
	FilledQty      string    `json:"filled_qty"`
	FilledAvgPrice string    `json:"filled_avg_price"`
	SubmittedAt    time.Time `json:"submitted_at"`
}

func (r alpacaOrder) toOrder() Order {
	side := domain.OrderBuy
	if r.Side == "sell" {
		side = domain.OrderSell
	}
	return Order{
		ID: r.ID, ClientOrderID: r.ClientOrderID, Ticker: domain.NormalizeTicker(r.Symbol),
		Side: side, Quantity: num(r.Qty), LimitPrice: num(r.LimitPrice),
		Status: alpacaStatus(r.Status), VenueStatus: r.Status,
		FilledQty: num(r.FilledQty), AvgFillPrice: num(r.FilledAvgPrice),
		SubmittedAt: r.SubmittedAt,
	}
}

// alpacaStatus maps Alpaca's order states onto the platform's.
func alpacaStatus(s string) domain.OrderStatus {
	switch s {
	case "filled":
		return domain.OrderFilled
	case "partially_filled":
		return domain.OrderPartial
	case "canceled", "expired", "done_for_day", "replaced":
		return domain.OrderCancelled
	case "rejected", "suspended", "stopped":
		return domain.OrderRejected
	default: // new, accepted, pending_new, accepted_for_bidding, calculated, held, pending_*
		return domain.OrderPending
	}
}

// Submit implements Venue. A duplicate client_order_id (a retried approval)
// returns the order that already exists rather than an error.
func (a *Alpaca) Submit(ctx context.Context, req OrderRequest) (Order, error) {
	if req.Quantity <= 0 || req.LimitPrice <= 0 {
		return Order{}, errors.New("execution: order needs a positive quantity and limit price")
	}
	side := "buy"
	if req.Side == domain.OrderSell {
		side = "sell"
	}
	tif := req.TimeInForce
	if tif == "" {
		tif = "day"
	}
	body := alpacaOrderReq{
		Symbol: string(req.Ticker), Qty: strconv.FormatFloat(req.Quantity, 'f', -1, 64),
		Side: side, Type: "limit", TimeInForce: tif,
		LimitPrice:    strconv.FormatFloat(req.LimitPrice, 'f', 2, 64),
		ClientOrderID: req.ClientOrderID,
	}
	var r alpacaOrder
	err := a.do(ctx, http.MethodPost, "/v2/orders", body, &r)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(apiErr.Message), "client_order_id") {
		return a.OrderByClientID(ctx, req.ClientOrderID)
	}
	if err != nil {
		return Order{}, err
	}
	return r.toOrder(), nil
}

// OrderByClientID implements Venue.
func (a *Alpaca) OrderByClientID(ctx context.Context, id string) (Order, error) {
	var r alpacaOrder
	err := a.do(ctx, http.MethodGet, "/v2/orders:by_client_order_id?client_order_id="+url.QueryEscape(id), nil, &r)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return Order{}, ErrOrderNotFound
	}
	if err != nil {
		return Order{}, err
	}
	return r.toOrder(), nil
}

// CancelAll implements Venue. Alpaca answers 207 with a per-order result; a
// 2xx means the request was accepted, which is what the kill switch needs.
func (a *Alpaca) CancelAll(ctx context.Context) error {
	return a.do(ctx, http.MethodDelete, "/v2/orders", nil, nil)
}

// Quote returns the current price of an instrument from Alpaca's market data:
// the bid/ask midpoint when both sides are quoted and the spread is sane, the
// last trade otherwise. These are real market prices, which is the point: the
// guardrails and the order's limit price must be measured against the market
// the order will actually meet, not against QuantOS's own feed.
func (a *Alpaca) Quote(ctx context.Context, t domain.Ticker) (float64, error) {
	sym := url.PathEscape(string(t))
	var q struct {
		Quote struct {
			Ask float64 `json:"ap"`
			Bid float64 `json:"bp"`
		} `json:"quote"`
	}
	if err := a.doAt(ctx, a.dataURL, http.MethodGet, "/v2/stocks/"+sym+"/quotes/latest?feed="+a.feed, nil, &q); err != nil {
		return 0, err
	}
	ask, bid := q.Quote.Ask, q.Quote.Bid
	if ask > 0 && bid > 0 && ask >= bid && (ask-bid)/ask < 0.01 {
		return math.Round((ask+bid)/2*100) / 100, nil
	}
	var tr struct {
		Trade struct {
			Price float64 `json:"p"`
		} `json:"trade"`
	}
	if err := a.doAt(ctx, a.dataURL, http.MethodGet, "/v2/stocks/"+sym+"/trades/latest?feed="+a.feed, nil, &tr); err != nil {
		return 0, err
	}
	if tr.Trade.Price <= 0 {
		return 0, fmt.Errorf("alpaca: no usable quote or trade for %s", t)
	}
	return tr.Trade.Price, nil
}
