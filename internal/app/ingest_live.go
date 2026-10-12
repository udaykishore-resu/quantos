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
	Sessions(ctx context.Context, from, to time.Time) ([]marketdata.Session, error)
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
	// sessions are the regular sessions known so far; sessionsTo is the
	// latest instant they are known to cover.
	sessions   []marketdata.Session
	sessionsTo time.Time
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
//
// Only regular-session bars are used. If the trading calendar cannot be read
// the warm-up is skipped rather than run on unfiltered bars.
func (li *liveIngest) warm(ctx context.Context) {
	cfg := li.a.Cfg
	days := cfg.Market.Alpaca.HistoryDays
	if days <= 0 {
		days = 5
	}
	now := li.a.Clock.Now()
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	if err := li.loadSessions(ctx, from, now); err != nil {
		li.a.Log.Error("live warm-up skipped: trading calendar unavailable", "error", err)
		return
	}
	all, err := li.feed.GetHistoricalBars(ctx, marketdata.BarRequest{
		Tickers: li.tickers, Interval: domain.Interval1m, Start: from, End: now,
	})
	if err != nil {
		li.a.Log.Error("live warm-up failed; indicators will warm from live bars instead", "error", err)
		return
	}
	keep := cfg.Features.MaxHistory
	if keep < cfg.Features.Warmup {
		keep = cfg.Features.Warmup
	}
	// Filter to the regular session, then keep the most recent `keep` bars per
	// symbol. Bars arrive in time order, so counting from the end is enough.
	inSession := make([]domain.Candle, 0, len(all))
	extended := 0
	for _, c := range all {
		if c.End.After(now) {
			continue // the minute in progress
		}
		if !li.inSession(c) {
			extended++
			continue
		}
		inSession = append(inSession, c)
	}
	counts := map[domain.Ticker]int{}
	for i := len(inSession) - 1; i >= 0; i-- {
		counts[inSession[i].Ticker]++
	}
	seen := map[domain.Ticker]int{}
	historical, used := 0, 0
	for _, c := range inSession {
		seen[c.Ticker]++
		if counts[c.Ticker]-seen[c.Ticker] >= keep {
			continue
		}
		used++
		res := li.a.Pipeline.OnBar(ctx, c)
		// The read model is updated so the dashboard shows the last real
		// state — over a weekend there is no live bar to do it until Monday —
		// but nothing decided on history is published: a signal or alert about
		// a bar from days ago is not news.
		if res.Signal != nil {
			historical++
		}
		res.Signal, res.Alerts, res.Invalidated = nil, nil, nil
		li.a.State.Observe(res)
		if c.End.After(li.lastBar[c.Ticker]) {
			li.lastBar[c.Ticker] = c.End
		}
	}
	if historical > 0 {
		li.a.Log.Info("warm-up produced signals on historical bars; they were not published", "count", historical)
	}
	short := 0
	for _, t := range li.tickers {
		if counts[t] < cfg.Features.Warmup {
			short++
		}
	}
	li.a.Log.Info("live feed warmed", "bars", used, "extended_hours_dropped", extended,
		"symbols", len(li.tickers), "symbols_short_of_warmup", short, "sessions", len(li.sessions), "days", days)
}

// loadSessions reads the trading calendar for [from, to].
func (li *liveIngest) loadSessions(ctx context.Context, from, to time.Time) error {
	ss, err := li.feed.Sessions(ctx, from, to)
	if err != nil {
		return err
	}
	li.sessions, li.sessionsTo = ss, to
	return nil
}

// inSession reports whether a bar lies inside a regular session.
func (li *liveIngest) inSession(c domain.Candle) bool {
	for i := len(li.sessions) - 1; i >= 0; i-- {
		if li.sessions[i].Contains(c) {
			return true
		}
	}
	return false
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
	// The calendar is re-read once a day, so today's session — including an
	// early close — is known before its bars are filtered against it.
	if now.Sub(li.sessionsTo) > 12*time.Hour || !li.sessionsCover(now) {
		if err := li.loadSessions(ctx, now.Add(-7*24*time.Hour), now.Add(24*time.Hour)); err != nil {
			li.warn("calendar", "trading calendar unavailable; no bars published until it returns", "error", err)
			return
		}
	}
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
		if !li.inSession(c) {
			continue // pre-market or after-hours
		}
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

// sessionsCover reports whether a known session contains t.
func (li *liveIngest) sessionsCover(t time.Time) bool {
	for _, ss := range li.sessions {
		if !t.Before(ss.Open) && !t.After(ss.Close) {
			return true
		}
	}
	return false
}
