package app

import (
	"context"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
)

// liveFeed is the part of a live provider the ingest loop uses. It is an
// interface so the loop can be tested without a network.
type liveFeed interface {
	GetQuotes(ctx context.Context, tickers []domain.Ticker) ([]domain.Quote, error)
	GetHistoricalBars(ctx context.Context, req marketdata.BarRequest) ([]domain.Candle, error)
	GetMarketStatus(ctx context.Context) (domain.MarketStatus, error)
	Excluded(t domain.Ticker) bool
}

// liveIngest polls a live provider and publishes what is new.
//
// The simulator pushes exactly one quote per symbol per tick and a bar every
// minute; a polled feed does not. A latest-quote endpoint returns the same
// quote until something trades, and the validator rightly rejects a quote that
// is not newer than the last one, so the loop publishes a quote only when its
// timestamp has advanced, and a bar only once its minute has closed.
type liveIngest struct {
	a       *App
	feed    liveFeed
	builder *bus.Builder
	tickers []domain.Ticker

	open       bool
	nextClock  time.Time
	nextBars   time.Time
	lastQuote  map[domain.Ticker]time.Time
	lastBar    map[domain.Ticker]time.Time
	lastWarned map[string]time.Time
}

func (a *App) newLiveIngest(feed liveFeed) *liveIngest {
	var tickers []domain.Ticker
	for _, s := range a.Universe.All() {
		if !feed.Excluded(s.Ticker) {
			tickers = append(tickers, s.Ticker)
		}
	}
	return &liveIngest{
		a: a, feed: feed, builder: bus.NewBuilder(a.Cfg.Service+"/ingest", a.Clock),
		tickers: tickers, lastQuote: map[domain.Ticker]time.Time{},
		lastBar: map[domain.Ticker]time.Time{}, lastWarned: map[string]time.Time{},
	}
}

// runLiveIngest drives a live provider until ctx is cancelled.
func (a *App) runLiveIngest(ctx context.Context, feed liveFeed) {
	li := a.newLiveIngest(feed)
	li.warm(ctx)
	every := a.Cfg.Market.TickInterval
	if every < 3*time.Second {
		every = 3 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		li.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// warn logs at most once a minute per key, so an outage is visible without
// becoming the whole log.
func (li *liveIngest) warn(key, msg string, args ...any) {
	now := li.a.Clock.Now()
	if now.Sub(li.lastWarned[key]) < time.Minute {
		return
	}
	li.lastWarned[key] = now
	li.a.Log.Warn(msg, args...)
}

// warm reads recent 1m history and feeds it straight to the pipeline, which is
// the warm-up any live process performs at start-up: the first live bar is
// then judged against real history rather than against nothing.
func (li *liveIngest) warm(ctx context.Context) {
	cfg := li.a.Cfg
	days := cfg.Market.Alpaca.HistoryDays
	if days <= 0 {
		days = 5
	}
	now := li.a.Clock.Now()
	limit := cfg.Features.MaxHistory
	if limit < cfg.Features.Warmup {
		limit = cfg.Features.Warmup
	}
	bars, err := li.feed.GetHistoricalBars(ctx, marketdata.BarRequest{
		Tickers: li.tickers, Interval: domain.Interval1m,
		Start: now.Add(-time.Duration(days) * 24 * time.Hour), End: now, Limit: limit,
	})
	if err != nil {
		li.a.Log.Error("live warm-up failed; indicators will warm from live bars instead", "error", err)
		return
	}
	for _, c := range bars {
		if c.End.After(now) {
			continue // the minute in progress
		}
		li.a.Pipeline.OnBar(ctx, c)
		if c.End.After(li.lastBar[c.Ticker]) {
			li.lastBar[c.Ticker] = c.End
		}
	}
	short := 0
	counts := map[domain.Ticker]int{}
	for _, c := range bars {
		counts[c.Ticker]++
	}
	for _, t := range li.tickers {
		if counts[t] < cfg.Features.Warmup {
			short++
		}
	}
	li.a.Log.Info("live feed warmed", "bars", len(bars), "symbols", len(li.tickers),
		"symbols_short_of_warmup", short, "days", days)
}

// step is one poll.
func (li *liveIngest) step(ctx context.Context) {
	now := li.a.Clock.Now()
	if !now.Before(li.nextClock) {
		st, err := li.feed.GetMarketStatus(ctx)
		if err != nil {
			li.warn("clock", "market clock unavailable; keeping the last known session", "error", err)
		} else {
			if (st.Session == domain.SessionOpen) != li.open {
				li.a.Log.Info("market session", "open", st.Session == domain.SessionOpen,
					"next_open", st.NextOpen, "next_close", st.NextClose)
			}
			li.open = st.Session == domain.SessionOpen
		}
		li.nextClock = now.Add(time.Minute)
	}
	if !li.open {
		// Nothing trades, so nothing is polled and nothing is stale: a closed
		// market is not a data outage.
		return
	}

	quotes, err := li.feed.GetQuotes(ctx, li.tickers)
	if err != nil {
		li.warn("quotes", "live quotes unavailable", "error", err)
	}
	fresh := make([]domain.Quote, 0, len(quotes))
	for _, q := range quotes {
		if !q.Timestamp.After(li.lastQuote[q.Ticker]) {
			continue
		}
		li.lastQuote[q.Ticker] = q.Timestamp
		if err := li.a.Validator.ValidateQuote(q); err != nil {
			li.a.publishRejected(ctx, li.builder, q.Ticker, err)
			continue
		}
		fresh = append(fresh, q)
	}
	if len(fresh) > 0 {
		li.a.publishQuotes(ctx, li.builder, fresh)
	}

	if !now.Before(li.nextBars) {
		li.pollBars(ctx, now)
		// Five seconds past the next minute, so the bar that just closed is
		// complete at the source before it is read.
		li.nextBars = now.Truncate(time.Minute).Add(time.Minute + 5*time.Second)
	}
	li.a.checkStaleness(ctx, li.builder)
}

func (li *liveIngest) pollBars(ctx context.Context, now time.Time) {
	from := now.Add(-10 * time.Minute)
	for _, t := range li.tickers {
		if last := li.lastBar[t]; !last.IsZero() && last.Before(from) && now.Sub(last) < time.Hour {
			from = last
		}
	}
	bars, err := li.feed.GetHistoricalBars(ctx, marketdata.BarRequest{
		Tickers: li.tickers, Interval: domain.Interval1m, Start: from, End: now,
	})
	if err != nil {
		li.warn("bars", "live bars unavailable", "error", err)
		return
	}
	for _, c := range bars {
		if c.End.After(now) || !c.End.After(li.lastBar[c.Ticker]) {
			continue
		}
		li.lastBar[c.Ticker] = c.End
		if err := li.a.Validator.ValidateCandle(c); err != nil {
			li.a.publishRejected(ctx, li.builder, c.Ticker, err)
			continue
		}
		env, err := li.builder.New(ctx, bus.TopicBars, string(c.Ticker), c.End, c)
		if err != nil {
			continue
		}
		if err := li.a.Bus.Publish(ctx, bus.TopicBars, env); err != nil {
			li.a.Metrics.BusErrors.Inc(bus.TopicBars, "publish")
		}
	}
}
