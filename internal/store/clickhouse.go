package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/domain"
)

// ClickHouse is the analytical store: append-only time series read in wide
// scans (ADR-003).
//
// It talks to ClickHouse's HTTP interface with JSONEachRow, which is a stable,
// documented protocol. Using it directly rather than a driver keeps the
// dependency surface small and makes the wire behaviour — batching, timeouts,
// compression — explicit at the call site rather than hidden behind a pool.
type ClickHouse struct {
	baseURL  string
	database string
	user     string
	password string
	client   *http.Client
	timeout  time.Duration
}

// ClickHouseConfig parameterises the connection.
type ClickHouseConfig struct {
	URL      string // e.g. http://clickhouse:8123
	Database string
	User     string
	Password string
	Timeout  time.Duration
}

// OpenClickHouse builds a client and verifies connectivity.
func OpenClickHouse(ctx context.Context, cfg ClickHouseConfig) (*ClickHouse, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("store: clickhouse URL is required")
	}
	if cfg.Database == "" {
		cfg.Database = "quantos"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	c := &ClickHouse{
		baseURL: strings.TrimRight(cfg.URL, "/"), database: cfg.Database,
		user: cfg.User, password: cfg.Password,
		client: &http.Client{Timeout: cfg.Timeout}, timeout: cfg.Timeout,
	}
	if err := c.Ping(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// exec runs a statement with an optional body.
func (c *ClickHouse) exec(ctx context.Context, query string, body io.Reader) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := c.baseURL + "/?" + url.Values{
		"database": {c.database},
		"query":    {query},
		// Bounded so a runaway analytical query cannot take the cluster down.
		"max_execution_time": {"30"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.Header.Set("X-ClickHouse-User", c.user)
		req.Header.Set("X-ClickHouse-Key", c.password)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: clickhouse: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("clickhouse: status %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// insert writes rows in JSONEachRow format.
func (c *ClickHouse) insert(ctx context.Context, table string, rows []any) error {
	if len(rows) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("store: encode row for %s: %w", table, err)
		}
	}
	_, err := c.exec(ctx, fmt.Sprintf("INSERT INTO %s FORMAT JSONEachRow", table), &buf)
	return err
}

// query runs a SELECT and decodes JSONEachRow output into dst rows.
func (c *ClickHouse) query(ctx context.Context, q string, fn func([]byte) error) error {
	out, err := c.exec(ctx, q+" FORMAT JSONEachRow", nil)
	if err != nil {
		return err
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return nil
}

// Ping verifies the server responds.
func (c *ClickHouse) Ping(ctx context.Context) error {
	_, err := c.exec(ctx, "SELECT 1", nil)
	return err
}

// Close is a no-op: the HTTP client holds no persistent resources beyond the
// shared transport.
func (c *ClickHouse) Close() error { return nil }

// chTime formats a timestamp for ClickHouse DateTime64(3, 'UTC').
func chTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000") }

type chQuote struct {
	Ticker  string  `json:"ticker"`
	TS      string  `json:"ts"`
	Bid     float64 `json:"bid"`
	Ask     float64 `json:"ask"`
	Last    float64 `json:"last"`
	BidSize float64 `json:"bid_size"`
	AskSize float64 `json:"ask_size"`
	Volume  float64 `json:"volume"`
	Seq     uint64  `json:"seq"`
	Source  string  `json:"source"`
}

// SaveQuotes appends quotes.
func (c *ClickHouse) SaveQuotes(ctx context.Context, qs []domain.Quote) error {
	rows := make([]any, 0, len(qs))
	for _, q := range qs {
		rows = append(rows, chQuote{
			Ticker: string(q.Ticker), TS: chTime(q.Timestamp), Bid: q.Bid, Ask: q.Ask,
			Last: q.Last, BidSize: q.BidSize, AskSize: q.AskSize, Volume: q.Volume,
			Seq: q.Seq, Source: q.Source,
		})
	}
	return c.insert(ctx, "quotes", rows)
}

type chCandle struct {
	Ticker   string  `json:"ticker"`
	Interval string  `json:"interval"`
	Start    string  `json:"start"`
	End      string  `json:"end"`
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	Volume   float64 `json:"volume"`
	VWAP     float64 `json:"vwap"`
	Trades   int64   `json:"trades"`
	Adjusted uint8   `json:"adjusted"`
	Source   string  `json:"source"`
}

// SaveCandles appends bars.
func (c *ClickHouse) SaveCandles(ctx context.Context, cs []domain.Candle) error {
	rows := make([]any, 0, len(cs))
	for _, x := range cs {
		adj := uint8(0)
		if x.Adjusted {
			adj = 1
		}
		rows = append(rows, chCandle{
			Ticker: string(x.Ticker), Interval: string(x.Interval),
			Start: chTime(x.Start), End: chTime(x.End),
			Open: x.Open, High: x.High, Low: x.Low, Close: x.Close,
			Volume: x.Volume, VWAP: x.VWAP, Trades: x.Trades, Adjusted: adj, Source: x.Source,
		})
	}
	return c.insert(ctx, "bars", rows)
}

// ListCandles reads bars for one symbol and interval.
func (c *ClickHouse) ListCandles(ctx context.Context, q Query, interval domain.Interval) ([]domain.Candle, error) {
	if q.Ticker == "" {
		return nil, fmt.Errorf("store: ListCandles requires a ticker")
	}
	where := []string{fmt.Sprintf("ticker = %s", quoteLiteral(string(q.Ticker)))}
	if interval != "" {
		where = append(where, fmt.Sprintf("interval = %s", quoteLiteral(string(interval))))
	}
	if !q.From.IsZero() {
		where = append(where, fmt.Sprintf("start >= toDateTime64(%s, 3)", quoteLiteral(chTime(q.From))))
	}
	if !q.To.IsZero() {
		where = append(where, fmt.Sprintf("start <= toDateTime64(%s, 3)", quoteLiteral(chTime(q.To))))
	}
	sql := fmt.Sprintf(
		"SELECT ticker, interval, toString(start) AS start, toString(end) AS end, open, high, low, close, volume, vwap, trades, adjusted, source FROM bars WHERE %s ORDER BY start ASC LIMIT %d",
		strings.Join(where, " AND "), limitOr(q.Limit, 5000))

	var out []domain.Candle
	err := c.query(ctx, sql, func(line []byte) error {
		var r chCandle
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		start, _ := time.Parse("2006-01-02 15:04:05.000", r.Start)
		end, _ := time.Parse("2006-01-02 15:04:05.000", r.End)
		out = append(out, domain.Candle{
			Ticker: domain.Ticker(r.Ticker), Interval: domain.Interval(r.Interval),
			Open: r.Open, High: r.High, Low: r.Low, Close: r.Close, Volume: r.Volume,
			VWAP: r.VWAP, Trades: r.Trades, Start: start.UTC(), End: end.UTC(),
			Adjusted: r.Adjusted == 1, Source: r.Source,
		})
		return nil
	})
	return out, err
}

// LatestQuote reads the most recent quote for a symbol.
func (c *ClickHouse) LatestQuote(ctx context.Context, t domain.Ticker) (domain.Quote, error) {
	sql := fmt.Sprintf(
		"SELECT ticker, toString(ts) AS ts, bid, ask, last, bid_size, ask_size, volume, seq, source FROM quotes WHERE ticker = %s ORDER BY ts DESC LIMIT 1",
		quoteLiteral(string(t)))
	var out domain.Quote
	found := false
	err := c.query(ctx, sql, func(line []byte) error {
		var r chQuote
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		ts, _ := time.Parse("2006-01-02 15:04:05.000", r.TS)
		out = domain.Quote{
			Ticker: domain.Ticker(r.Ticker), Bid: r.Bid, Ask: r.Ask, Last: r.Last,
			BidSize: r.BidSize, AskSize: r.AskSize, Volume: r.Volume,
			Timestamp: ts.UTC(), Seq: r.Seq, Source: r.Source,
		}
		found = true
		return nil
	})
	if err != nil {
		return domain.Quote{}, err
	}
	if !found {
		return domain.Quote{}, ErrNotFound
	}
	return out, nil
}

type chSnapshot struct {
	Ticker   string             `json:"ticker"`
	AsOf     string             `json:"as_of"`
	Interval string             `json:"interval"`
	Hash     string             `json:"hash"`
	Last     float64            `json:"last"`
	Bid      float64            `json:"bid"`
	Ask      float64            `json:"ask"`
	Volume   float64            `json:"volume"`
	Spread   float64            `json:"spread_bps"`
	Stale    uint8              `json:"stale"`
	Values   map[string]float64 `json:"values"`
	Warm     map[string]uint8   `json:"warm"`
}

// SaveSnapshot appends a feature snapshot.
func (c *ClickHouse) SaveSnapshot(ctx context.Context, s domain.FeatureSnapshot) error {
	warm := make(map[string]uint8, len(s.Warm))
	for k, v := range s.Warm {
		if v {
			warm[k] = 1
		} else {
			warm[k] = 0
		}
	}
	stale := uint8(0)
	if s.Stale {
		stale = 1
	}
	return c.insert(ctx, "feature_snapshots", []any{chSnapshot{
		Ticker: string(s.Ticker), AsOf: chTime(s.AsOf), Interval: string(s.Interval),
		Hash: s.Hash, Last: s.Last, Bid: s.Bid, Ask: s.Ask, Volume: s.Volume,
		Spread: s.SpreadBps, Stale: stale, Values: s.Values, Warm: warm,
	}})
}

// GetSnapshot reads a snapshot by ticker and hash, which is the reproducibility
// lookup: a stored hash on a signal resolves to the exact inputs that produced it.
func (c *ClickHouse) GetSnapshot(ctx context.Context, ticker domain.Ticker, hash string) (domain.FeatureSnapshot, error) {
	sql := fmt.Sprintf(
		"SELECT ticker, toString(as_of) AS as_of, interval, hash, last, bid, ask, volume, spread_bps, stale, values, warm FROM feature_snapshots WHERE ticker = %s AND hash = %s LIMIT 1",
		quoteLiteral(string(ticker)), quoteLiteral(hash))
	var out domain.FeatureSnapshot
	found := false
	err := c.query(ctx, sql, func(line []byte) error {
		var r chSnapshot
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		asOf, _ := time.Parse("2006-01-02 15:04:05.000", r.AsOf)
		warm := make(map[string]bool, len(r.Warm))
		for k, v := range r.Warm {
			warm[k] = v == 1
		}
		out = domain.FeatureSnapshot{
			Ticker: domain.Ticker(r.Ticker), AsOf: asOf.UTC(),
			Interval: domain.Interval(r.Interval), Values: r.Values, Warm: warm,
			Last: r.Last, Bid: r.Bid, Ask: r.Ask, Volume: r.Volume,
			SpreadBps: r.Spread, Stale: r.Stale == 1, Hash: r.Hash,
			SchemaVersion: domain.FeatureSchemaVersion,
		}
		found = true
		return nil
	})
	if err != nil {
		return out, err
	}
	if !found {
		return out, ErrNotFound
	}
	return out, nil
}

// ListSnapshots reads recent snapshots for a symbol.
func (c *ClickHouse) ListSnapshots(ctx context.Context, q Query) ([]domain.FeatureSnapshot, error) {
	where := []string{"1"}
	if q.Ticker != "" {
		where = append(where, fmt.Sprintf("ticker = %s", quoteLiteral(string(q.Ticker))))
	}
	if !q.From.IsZero() {
		where = append(where, fmt.Sprintf("as_of >= toDateTime64(%s, 3)", quoteLiteral(chTime(q.From))))
	}
	sql := fmt.Sprintf(
		"SELECT ticker, toString(as_of) AS as_of, interval, hash, last, bid, ask, volume, spread_bps, stale, values, warm FROM feature_snapshots WHERE %s ORDER BY as_of DESC LIMIT %d",
		strings.Join(where, " AND "), limitOr(q.Limit, 500))
	var out []domain.FeatureSnapshot
	err := c.query(ctx, sql, func(line []byte) error {
		var r chSnapshot
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		asOf, _ := time.Parse("2006-01-02 15:04:05.000", r.AsOf)
		warm := make(map[string]bool, len(r.Warm))
		for k, v := range r.Warm {
			warm[k] = v == 1
		}
		out = append(out, domain.FeatureSnapshot{
			Ticker: domain.Ticker(r.Ticker), AsOf: asOf.UTC(),
			Interval: domain.Interval(r.Interval), Values: r.Values, Warm: warm,
			Last: r.Last, Hash: r.Hash, Stale: r.Stale == 1,
		})
		return nil
	})
	return out, err
}

type chPrediction struct {
	ID           string  `json:"id"`
	Ticker       string  `json:"ticker"`
	CreatedAt    string  `json:"created_at"`
	ModelVersion string  `json:"model_version"`
	Source       string  `json:"source"`
	FeatureHash  string  `json:"feature_hash"`
	Regime       string  `json:"regime"`
	Payload      string  `json:"payload"`
	PrimaryUp    float64 `json:"primary_up"`
	PrimaryFlat  float64 `json:"primary_flat"`
	PrimaryDown  float64 `json:"primary_down"`
	Confidence   float64 `json:"confidence"`
}

// SavePrediction appends a prediction.
func (c *ClickHouse) SavePrediction(ctx context.Context, p domain.Prediction) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	pr := p.Primary()
	return c.insert(ctx, "predictions", []any{chPrediction{
		ID: p.ID, Ticker: string(p.Ticker), CreatedAt: chTime(p.CreatedAt),
		ModelVersion: p.ModelVersion, Source: string(p.Source), FeatureHash: p.FeatureHash,
		Regime: string(p.Regime), Payload: string(payload),
		PrimaryUp: pr.Dist.Up, PrimaryFlat: pr.Dist.Flat, PrimaryDown: pr.Dist.Down,
		Confidence: pr.Confidence,
	}})
}

// GetPrediction reads a prediction by id.
func (c *ClickHouse) GetPrediction(ctx context.Context, id string) (domain.Prediction, error) {
	sql := fmt.Sprintf("SELECT payload FROM predictions WHERE id = %s LIMIT 1", quoteLiteral(id))
	var out domain.Prediction
	found := false
	err := c.query(ctx, sql, func(line []byte) error {
		var row struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(row.Payload), &out); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return out, err
	}
	if !found {
		return out, ErrNotFound
	}
	return out, nil
}

// ListPredictions reads recent predictions.
func (c *ClickHouse) ListPredictions(ctx context.Context, q Query) ([]domain.Prediction, error) {
	where := []string{"1"}
	if q.Ticker != "" {
		where = append(where, fmt.Sprintf("ticker = %s", quoteLiteral(string(q.Ticker))))
	}
	if !q.From.IsZero() {
		where = append(where, fmt.Sprintf("created_at >= toDateTime64(%s, 3)", quoteLiteral(chTime(q.From))))
	}
	sql := fmt.Sprintf("SELECT payload FROM predictions WHERE %s ORDER BY created_at DESC LIMIT %d",
		strings.Join(where, " AND "), limitOr(q.Limit, 500))
	var out []domain.Prediction
	err := c.query(ctx, sql, func(line []byte) error {
		var row struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		var p domain.Prediction
		if err := json.Unmarshal([]byte(row.Payload), &p); err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

type chOutcome struct {
	PredictionID string  `json:"prediction_id"`
	Ticker       string  `json:"ticker"`
	Horizon      string  `json:"horizon"`
	CreatedAt    string  `json:"created_at"`
	ResolvedAt   string  `json:"resolved_at"`
	EntryPrice   float64 `json:"entry_price"`
	ExitPrice    float64 `json:"exit_price"`
	ReturnBps    float64 `json:"return_bps"`
	Predicted    string  `json:"predicted"`
	Actual       string  `json:"actual"`
	Correct      uint8   `json:"correct"`
	Brier        float64 `json:"brier_score"`
	LogLoss      float64 `json:"log_loss"`
	Regime       string  `json:"regime"`
	ModelVersion string  `json:"model_version"`
	RiskDecision string  `json:"risk_decision"`
	PUp          float64 `json:"p_up"`
	PFlat        float64 `json:"p_flat"`
	PDown        float64 `json:"p_down"`
}

// SaveOutcome appends a resolved prediction outcome.
func (c *ClickHouse) SaveOutcome(ctx context.Context, o domain.PredictionOutcome) error {
	correct := uint8(0)
	if o.Correct {
		correct = 1
	}
	return c.insert(ctx, "prediction_outcomes", []any{chOutcome{
		PredictionID: o.PredictionID, Ticker: string(o.Ticker), Horizon: string(o.Horizon),
		CreatedAt: chTime(o.CreatedAt), ResolvedAt: chTime(o.ResolvedAt),
		EntryPrice: o.EntryPrice, ExitPrice: o.ExitPrice, ReturnBps: o.ReturnBps,
		Predicted: string(o.Predicted), Actual: string(o.Actual), Correct: correct,
		Brier: o.BrierScore, LogLoss: o.LogLoss, Regime: string(o.Regime),
		ModelVersion: o.ModelVersion, RiskDecision: string(o.RiskDecision),
		PUp: o.Dist.Up, PFlat: o.Dist.Flat, PDown: o.Dist.Down,
	}})
}

// ListOutcomes reads resolved outcomes.
func (c *ClickHouse) ListOutcomes(ctx context.Context, q Query) ([]domain.PredictionOutcome, error) {
	where := []string{"1"}
	if q.Ticker != "" {
		where = append(where, fmt.Sprintf("ticker = %s", quoteLiteral(string(q.Ticker))))
	}
	if !q.From.IsZero() {
		where = append(where, fmt.Sprintf("resolved_at >= toDateTime64(%s, 3)", quoteLiteral(chTime(q.From))))
	}
	sql := fmt.Sprintf(`SELECT prediction_id, ticker, horizon, toString(created_at) AS created_at,
 toString(resolved_at) AS resolved_at, entry_price, exit_price, return_bps, predicted, actual,
 correct, brier_score, log_loss, regime, model_version, risk_decision, p_up, p_flat, p_down
FROM prediction_outcomes WHERE %s ORDER BY resolved_at DESC LIMIT %d`,
		strings.Join(where, " AND "), limitOr(q.Limit, 5000))

	var out []domain.PredictionOutcome
	err := c.query(ctx, sql, func(line []byte) error {
		var r chOutcome
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		created, _ := time.Parse("2006-01-02 15:04:05.000", r.CreatedAt)
		resolved, _ := time.Parse("2006-01-02 15:04:05.000", r.ResolvedAt)
		out = append(out, domain.PredictionOutcome{
			PredictionID: r.PredictionID, Ticker: domain.Ticker(r.Ticker),
			Horizon: domain.Horizon(r.Horizon), CreatedAt: created.UTC(), ResolvedAt: resolved.UTC(),
			EntryPrice: r.EntryPrice, ExitPrice: r.ExitPrice, ReturnBps: r.ReturnBps,
			Predicted: domain.Outcome(r.Predicted), Actual: domain.Outcome(r.Actual),
			Correct: r.Correct == 1, Dist: domain.Distribution{Up: r.PUp, Flat: r.PFlat, Down: r.PDown},
			BrierScore: r.Brier, LogLoss: r.LogLoss, Regime: domain.Regime(r.Regime),
			ModelVersion: r.ModelVersion, RiskDecision: domain.RiskDecision(r.RiskDecision),
		})
		return nil
	})
	return out, err
}

// quoteLiteral escapes a string for inclusion in a ClickHouse query.
//
// The inputs here are tickers, hashes and formatted timestamps, all of which
// are already constrained upstream — but constructing SQL by concatenation
// without escaping is a habit worth not having, so every literal goes through
// this function.
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
