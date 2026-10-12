package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/udaykishoreresu/quantos/internal/bus"
	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/execution"
)

// buildExecution wires the approval desk when execution.enabled is set.
//
// A misconfigured desk does not stop the platform: it is left unbuilt and
// marked degraded, so research keeps working and nothing can reach the venue.
func (a *App) buildExecution(ctx context.Context, cfg config.Config, log *slog.Logger) {
	e := cfg.Execution
	if !e.Enabled {
		return
	}
	fail := func(err error) {
		log.Error("execution desk not started; no order can be placed until this is fixed", "error", err)
		a.markDegraded("execution")
	}
	keyID, secret := config.Secret(e.KeyIDEnv), config.Secret(e.SecretKeyEnv)
	if keyID == "" || secret == "" {
		fail(fmt.Errorf("set %s and %s to the venue's paper API keys", e.KeyIDEnv, e.SecretKeyEnv))
		return
	}
	venue, err := execution.NewAlpacaPaper(execution.AlpacaConfig{KeyID: keyID, SecretKey: secret, Feed: e.DataFeed})
	if err != nil {
		fail(err)
		return
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = venue.Account(cctx)
	cancel()
	var apiErr *execution.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
		fail(fmt.Errorf("Alpaca rejected the keys in %s/%s (HTTP %d); use paper-account keys", e.KeyIDEnv, e.SecretKeyEnv, apiErr.Status))
		return
	}
	if err != nil {
		log.Warn("execution venue unreachable at start-up; approvals will fail their account check until it returns", "error", err)
	}

	var st execution.Store
	if a.Postgres != nil {
		pg := execution.NewPostgresStore(a.Postgres.DB())
		if err := pg.EnsureSchema(ctx); err != nil {
			fail(err)
			return
		}
		st = pg
	} else {
		log.Warn("execution tickets are held in memory: the audit trail and the kill switch will not survive a restart",
			"store_driver", cfg.Store.Driver)
		st = execution.NewMemoryStore()
	}

	allow := make([]domain.Ticker, 0, len(e.Allowlist))
	for _, t := range e.Allowlist {
		allow = append(allow, domain.NormalizeTicker(t))
	}
	desk, err := execution.NewDesk(ctx, execution.DeskConfig{
		Venue: venue, Store: st, Clock: a.Clock, Logger: log,
		Quote: venue.Quote,
		Health: func(context.Context) (bool, string) {
			h, _ := a.ModelHealth()
			switch {
			case h.ModelVersion == "":
				return false, "no model health has been recorded yet"
			case h.Provisional && !e.AllowProvisionalModel:
				return false, "model health is provisional (measured offline, not on live outcomes)"
			case !h.Healthy:
				return false, "model unhealthy: " + strings.Join(h.Issues, "; ")
			}
			return true, fmt.Sprintf("model %s healthy: accuracy %.1f%%, calibration %.2f over %d outcomes",
				h.ModelVersion, h.Accuracy*100, h.Calibration, h.Samples)
		},
		Limits: execution.Limits{
			Allowlist: allow, MaxOrderNotional: e.MaxOrderNotional,
			MaxPositionWeight: e.MaxPositionWeight, MaxDailyLossPct: e.MaxDailyLossPct,
			MaxDriftBps: e.MaxDriftBps, TicketTTL: e.TicketTTL,
		},
	})
	if err != nil {
		fail(err)
		return
	}
	proposer, err := execution.NewProposer(execution.ProposerConfig{
		Desk: desk, Quote: venue.Quote, LimitBufferBps: e.LimitBufferBps, Clock: a.Clock,
	})
	if err != nil {
		fail(err)
		return
	}
	a.Desk, a.Proposer = desk, proposer
	if h := desk.HaltState(); h.Halted {
		log.Warn("execution desk starts halted", "reason", h.Reason, "by", h.By, "at", h.At)
	}
	log.Info("execution desk ready", "venue", venue.Name(), "allowlist", e.Allowlist,
		"auto_propose", e.AutoPropose, "max_order_notional", e.MaxOrderNotional)
}

// registerExecutionConsumers subscribes the desk to the signal stream: new
// signals become proposals (when auto_propose is on), and invalidated signals
// withdraw any proposal still waiting on them.
func (a *App) registerExecutionConsumers() error {
	if a.Desk == nil {
		return nil
	}
	group := a.Cfg.Bus.GroupPrefix + ".execution"
	if err := a.Bus.Subscribe(bus.TopicSignalInvalidated, group, func(ctx context.Context, env bus.Envelope) error {
		var s domain.Signal
		if err := env.Decode(&s); err != nil || s.ID == "" {
			return nil
		}
		a.Desk.CancelForSignal(ctx, s.ID, orDefault(s.InvalidationNote, string(s.InvalidationKind)))
		return nil
	}); err != nil {
		return err
	}
	if !a.Cfg.Execution.AutoPropose {
		return nil
	}
	return a.Bus.Subscribe(bus.TopicSignal, group, func(ctx context.Context, env bus.Envelope) error {
		var s domain.Signal
		if err := env.Decode(&s); err != nil {
			return nil
		}
		t, err := a.Proposer.Propose(ctx, s)
		switch {
		case err == nil:
			a.Log.Info("order ticket proposed", "ticket", t.ID, "signal", s.ID,
				"ticker", t.Ticker, "side", t.Side, "qty", t.Quantity, "limit", t.LimitPrice)
		case errors.Is(err, execution.ErrSkipped), errors.Is(err, execution.ErrDuplicateSignal):
			a.Log.Debug("signal not proposed", "signal", s.ID, "reason", err)
		case errors.Is(err, execution.ErrHalted):
			a.Log.Info("signal not proposed: trading is halted", "signal", s.ID)
		default:
			// A venue or store failure. Not retried: by the time it would
			// succeed the price has moved, and a stale proposal is worse than
			// a missing one.
			a.Log.Warn("signal could not be proposed", "signal", s.ID, "error", err)
		}
		return nil
	})
}

// runReconcile keeps every working order's ticket in step with the venue.
func (a *App) runReconcile(ctx context.Context) {
	every := a.Cfg.Execution.ReconcileInterval
	if every <= 0 {
		every = 30 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := a.Desk.Reconcile(ctx); err != nil && ctx.Err() == nil {
			a.Log.Warn("execution reconcile", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
