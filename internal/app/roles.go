package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/httpx"
)

// Additional roles for the split deployment. Each one is genuinely separate
// work, not a shim: the decision path itself stays co-located (ADR-006), while
// re-assessment, portfolio marking, alert delivery and backtest execution have
// different scaling and failure characteristics and are worth isolating.
const (
	// RolePortfolio marks the paper book and persists portfolio snapshots.
	RolePortfolio Role = "portfolio"
	// RoleAlertDelivery consumes alert.generated and dispatches to sinks.
	RoleAlertDelivery Role = "alert-delivery"
	// RoleBacktest executes queued backtest runs.
	RoleBacktest Role = "backtest"
)

// AlertSink delivers an alert somewhere outside the platform.
type AlertSink interface {
	Name() string
	Deliver(ctx context.Context, a domain.Alert) error
}

// LogSink writes alerts to the structured log. It is the default sink, so a
// deployment with no webhook configured still has a durable record.
type LogSink struct{ App *App }

// Name implements AlertSink.
func (s LogSink) Name() string { return "log" }

// Deliver implements AlertSink.
func (s LogSink) Deliver(_ context.Context, a domain.Alert) error {
	s.App.Log.Info("alert",
		"type", a.Type, "severity", a.Severity, "ticker", a.Ticker,
		"title", a.Title, "message", a.Message, "status", a.Status,
		"signal_id", a.SignalID, "dedup_key", a.DedupKey)
	return nil
}

// WebhookSink posts alerts to an HTTP endpoint.
type WebhookSink struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
}

// Name implements AlertSink.
func (s WebhookSink) Name() string { return "webhook" }

// Deliver implements AlertSink.
func (s WebhookSink) Deliver(ctx context.Context, a domain.Alert) error {
	if s.URL == "" {
		return nil
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, s.URL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("alert webhook returned %d", resp.StatusCode)
	}
	return nil
}

// RunAlertDelivery consumes alert.generated and dispatches to the sinks.
//
// Delivery is idempotent on the alert's dedup key, so a redelivered event does
// not page anyone twice — which is the whole reason the dedup key exists.
func (a *App) RunAlertDelivery(ctx context.Context, sinks ...AlertSink) error {
	if len(sinks) == 0 {
		sinks = []AlertSink{LogSink{App: a}}
	}
	group := a.Cfg.Bus.GroupPrefix + ".alert-delivery"
	handler := bus.Idempotent(a.Deduper, bus.IdempotentOptions{
		Group: group, TTL: a.Cfg.Bus.DedupTTL, Metrics: a.Metrics, Topic: bus.TopicAlert,
	}, func(ctx context.Context, e bus.Envelope) error {
		var al domain.Alert
		if err := e.Decode(&al); err != nil {
			return err
		}
		var firstErr error
		for _, s := range sinks {
			if err := s.Deliver(ctx, al); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("sink %s: %w", s.Name(), err)
			}
		}
		return firstErr
	})
	if err := a.Bus.Subscribe(bus.TopicAlert, group, handler); err != nil {
		return err
	}
	return a.Bus.Run(ctx)
}

// RunPortfolio marks the paper book on a schedule and persists snapshots.
//
// It is separate from the invalidation sweeper because marking is cheap and
// frequent while re-assessment is expensive and less frequent; running them at
// one cadence would mean choosing the wrong one for both.
func (a *App) RunPortfolio(ctx context.Context) error {
	interval := a.Cfg.Paper.MarkInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		now := a.Clock.Now()
		prices, volumes := a.Pipeline.Marks()
		a.Broker.Mark(prices, volumes, now)
		pf := a.Broker.Portfolio()
		if a.Store != nil {
			if err := a.Store.SavePortfolio(ctx, pf); err != nil {
				a.Log.Warn("could not persist portfolio", "error", err)
			}
			for _, o := range a.Broker.Orders() {
				if o.Status == domain.OrderFilled || o.Status == domain.OrderPartial {
					_, _ = a.Store.SaveOrder(ctx, o)
				}
			}
		}
		if a.Hub != nil {
			a.Hub.Publish(httpx.StreamPortfolio, pf)
		}
	}
}

// RunBacktestWorker keeps the process alive for the backtest runner, which
// executes submitted runs on its own goroutines. It exists as a role so a
// deployment can scale backtest capacity independently of the live path — a
// parameter sweep must never contend with signal generation for CPU.
func (a *App) RunBacktestWorker(ctx context.Context) error {
	a.Log.Info("backtest worker ready", "concurrency", 2)
	<-ctx.Done()
	return nil
}
