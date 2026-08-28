package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/udaykishoreresu/quantos/internal/auth"
	"github.com/udaykishoreresu/quantos/internal/backtest"
	"github.com/udaykishoreresu/quantos/internal/brief"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/evaluation"
	"github.com/udaykishoreresu/quantos/internal/httpx"
	"github.com/udaykishoreresu/quantos/internal/mlinfer"
	"github.com/udaykishoreresu/quantos/internal/obs"
	"github.com/udaykishoreresu/quantos/internal/paper"
	"github.com/udaykishoreresu/quantos/internal/rules"
	"github.com/udaykishoreresu/quantos/internal/signal"
	"github.com/udaykishoreresu/quantos/internal/store"
	"github.com/udaykishoreresu/quantos/internal/universe"
)

// Deps are the API's collaborators.
type Deps struct {
	State      *State
	Store      store.Store
	Universe   *universe.Universe
	Signals    *signal.Engine
	Broker     *paper.Broker
	Strategies *rules.Registry
	Models     *mlinfer.Registry
	Evaluator  *evaluation.Scheduler
	Brief      *brief.Generator
	Hub        *httpx.Hub
	Auth       *auth.Authenticator
	Backtests  *BacktestRunner
	Clock      obs.Clock
	Metrics    *obs.Metrics
	Logger     *slog.Logger
	Version    string
	Mode       string
	// Limiter and its budget are supplied rather than constructed here, because
	// rate limiting must run *after* authentication to key on the principal.
	// Mounted at the root router it would key on the client address instead.
	Limiter        httpx.Limiter
	RateLimitRPS   float64
	RateLimitBurst int
	// Degraded reports which subsystems are currently unavailable, so every
	// response can say so rather than quietly returning partial data.
	Degraded func() []string
	// Ready reports whether the service can serve its purpose.
	Ready func() (bool, string)
}

// API holds the handlers.
type API struct {
	d     Deps
	start time.Time
}

// New builds the API.
func New(d Deps) *API {
	if d.Clock == nil {
		d.Clock = obs.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &API{d: d, start: d.Clock.Now()}
}

func (a *API) degraded() []string {
	if a.d.Degraded == nil {
		return nil
	}
	return a.d.Degraded()
}

// Mount registers every route on the router.
func (a *API) Mount(r chi.Router) {
	r.Get("/healthz", a.health)
	r.Get("/readyz", a.ready)
	if a.d.Metrics != nil {
		r.Handle("/metrics", a.d.Metrics.Registry.Handler())
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", a.login)

		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(a.d.Auth, false))
			// Inside the authenticated group, so the bucket is per principal.
			r.Use(httpx.RateLimit(a.d.Limiter, a.d.RateLimitRPS, a.d.RateLimitBurst, a.d.Clock, a.d.Metrics))

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopeReadMarket))
				r.Get("/market/regime", a.regime)
				r.Get("/market/regime/history", a.regimeHistory)
				r.Get("/market/relationships", a.relationships)
				r.Get("/market/status", a.marketStatus)
				r.Get("/stocks", a.listStocks)
				r.Get("/stocks/{ticker}", a.getStock)
				r.Get("/stocks/{ticker}/candles", a.candles)
				r.Get("/news", a.news)
				r.Get("/brief", a.brief)
			})

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopeReadSignals))
				r.Get("/signals", a.listSignals)
				r.Get("/signals/{id}", a.getSignal)
				r.Get("/signals/{id}/provenance", a.provenance)
				r.Get("/predictions", a.listPredictions)
				r.Get("/predictions/{id}", a.getPrediction)
				r.Get("/alerts", a.listAlerts)
				r.Get("/risk/{ticker}", a.riskFor)
			})

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopeReadModels))
				r.Get("/models", a.listModels)
				r.Get("/model-health", a.modelHealth)
				r.Get("/evaluations", a.evaluations)
				r.Get("/strategies", a.listStrategies)
			})

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopeReadPortfolio))
				r.Get("/paper/portfolio", a.portfolio)
				r.Get("/paper/orders", a.orders)
				r.Get("/paper/positions", a.positions)
			})

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopeRunBacktest))
				r.Post("/backtests", a.createBacktest)
				r.Get("/backtests", a.listBacktests)
				r.Get("/backtests/{id}", a.getBacktest)
			})

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopePaperTrade))
				r.Post("/paper/orders", a.createOrder)
			})

			r.Group(func(r chi.Router) {
				r.Use(httpx.RequireScope(auth.ScopeReadAudit))
				r.Get("/audit", a.audit)
			})
		})

		// The stream accepts a query token because EventSource cannot set
		// headers; the allowance is scoped to this route alone.
		r.Group(func(r chi.Router) {
			r.Use(httpx.Authenticate(a.d.Auth, true))
			r.Use(httpx.RequireScope(auth.ScopeReadMarket))
			if a.d.Hub != nil {
				r.Get("/stream", a.d.Hub.Handler(20*time.Second))
			}
		})
	})
}

// --- Health ----------------------------------------------------------------

type healthResponse struct {
	Status    string        `json:"status"`
	Version   string        `json:"version"`
	Mode      string        `json:"mode"`
	Uptime    string        `json:"uptime"`
	Degraded  []string      `json:"degraded,omitempty"`
	Store     *store.Health `json:"store,omitempty"`
	Streams   int           `json:"stream_clients"`
	Timestamp time.Time     `json:"timestamp"`
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status: "ok", Version: a.d.Version, Mode: a.d.Mode,
		Uptime:   a.d.Clock.Now().Sub(a.start).Truncate(time.Second).String(),
		Degraded: a.degraded(), Timestamp: a.d.Clock.Now(),
	}
	if a.d.Hub != nil {
		resp.Streams = a.d.Hub.Clients()
	}
	if a.d.Store != nil {
		h := a.d.Store.Health(r.Context())
		resp.Store = &h
	}
	if len(resp.Degraded) > 0 {
		resp.Status = "degraded"
	}
	httpx.Respond(w, r, http.StatusOK, resp, nil, resp.Degraded)
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if a.d.Ready == nil {
		httpx.Respond(w, r, http.StatusOK, map[string]string{"status": "ready"}, nil, a.degraded())
		return
	}
	ok, reason := a.d.Ready()
	if !ok {
		httpx.Fail(w, r, httpx.ErrUnavailable.WithDetail(reason))
		return
	}
	httpx.Respond(w, r, http.StatusOK, map[string]string{"status": "ready"}, nil, a.degraded())
}

// --- Auth ------------------------------------------------------------------

type loginRequest struct {
	Subject  string `json:"subject"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string         `json:"token"`
	Principal auth.Principal `json:"principal"`
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, 4096, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if a.d.Auth == nil || !a.d.Auth.Enabled() {
		httpx.Respond(w, r, http.StatusOK, loginResponse{Principal: auth.Anonymous()}, nil, nil)
		return
	}
	token, p, err := a.d.Auth.Login(req.Subject, req.Password)
	if err != nil {
		// Deliberately vague: distinguishing "no such user" from "wrong
		// password" hands an attacker a user enumeration oracle.
		httpx.Fail(w, r, httpx.ErrUnauthorized.WithDetail("invalid credentials"))
		return
	}
	httpx.Respond(w, r, http.StatusOK, loginResponse{Token: token, Principal: p}, nil, nil)
}

// --- Market ----------------------------------------------------------------

func (a *API) regime(w http.ResponseWriter, r *http.Request) {
	rg := a.d.State.Regime()
	httpx.Respond(w, r, http.StatusOK, rg, httpx.At(rg.AsOf), a.degraded())
}

func (a *API) regimeHistory(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	q := store.Query{Limit: intParam(r, "limit", 100), From: timeParam(r, "from")}
	out, err := a.d.Store.ListRegimes(r.Context(), q)
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: q.Limit}, a.degraded())
}

func (a *API) relationships(w http.ResponseWriter, r *http.Request) {
	rel := a.d.State.Relationship()
	httpx.Respond(w, r, http.StatusOK, rel, httpx.At(rel.AsOf), a.degraded())
}

func (a *API) marketStatus(w http.ResponseWriter, r *http.Request) {
	rg := a.d.State.Regime()
	snaps := a.d.State.Snapshots()
	stale := 0
	for _, s := range snaps {
		if s.Stale {
			stale++
		}
	}
	httpx.Respond(w, r, http.StatusOK, map[string]any{
		"regime":            rg.Regime,
		"regime_confidence": rg.Confidence,
		"instruments":       len(snaps),
		"stale_instruments": stale,
		"as_of":             rg.AsOf,
	}, nil, a.degraded())
}

func (a *API) listStocks(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	sector := r.URL.Query().Get("sector")
	ranked := a.d.State.Rank(a.d.Universe.Get, sector, limit)
	httpx.Respond(w, r, http.StatusOK, ranked, &httpx.Meta{Count: len(ranked), Limit: limit}, a.degraded())
}

func (a *API) getStock(w http.ResponseWriter, r *http.Request) {
	t := domain.NormalizeTicker(chi.URLParam(r, "ticker"))
	stock, ok := a.d.Universe.Get(t)
	if !ok {
		httpx.Fail(w, r, httpx.ErrNotFound.WithDetail("unknown ticker "+string(t)))
		return
	}
	var signals []domain.Signal
	if a.d.Signals != nil {
		signals = a.d.Signals.ActiveFor(t)
	}
	view := a.d.State.View(stock, signals)
	meta := httpx.At(view.UpdatedAt)
	if view.Snapshot != nil {
		meta.Stale = view.Snapshot.Stale
		meta.StaleReason = view.Snapshot.StaleReason
	}
	httpx.Respond(w, r, http.StatusOK, view, meta, a.degraded())
}

func (a *API) candles(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	t := domain.NormalizeTicker(chi.URLParam(r, "ticker"))
	interval := domain.Interval(orDefault(r.URL.Query().Get("interval"), string(domain.Interval1m)))
	q := store.Query{Ticker: t, Limit: intParam(r, "limit", 500), From: timeParam(r, "from"), To: timeParam(r, "to")}
	out, err := a.d.Store.ListCandles(r.Context(), q, interval)
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: q.Limit}, a.degraded())
}

func (a *API) news(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 50)
	if t := r.URL.Query().Get("ticker"); t != "" && a.d.Store != nil {
		out, err := a.d.Store.ListNews(r.Context(), store.Query{
			Ticker: domain.NormalizeTicker(t), Limit: limit,
		})
		if err == nil {
			httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
			return
		}
	}
	out := a.d.State.AllNews(limit)
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: limit}, a.degraded())
}

// --- Signals ---------------------------------------------------------------

func (a *API) listSignals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := intParam(r, "limit", 100)

	// Active signals come from the engine, which is authoritative for the live
	// set; history comes from the store.
	if status == "" || status == string(domain.SignalActive) {
		if a.d.Signals != nil {
			// The engine holds the authoritative live set, but it does not
			// index it, so the same filters the store applies are applied here
			// by hand. Silently ignoring ?ticker= would be worse than not
			// supporting it: a caller would believe it had filtered.
			ticker := domain.NormalizeTicker(r.URL.Query().Get("ticker"))
			strategy := r.URL.Query().Get("strategy")
			from := timeParam(r, "from")
			offset := intParam(r, "offset", 0)

			var out []domain.Signal
			for _, sig := range a.d.Signals.Active() {
				if ticker != "" && sig.Ticker != ticker {
					continue
				}
				if strategy != "" && sig.StrategyID != strategy {
					continue
				}
				if !from.IsZero() && sig.CreatedAt.Before(from) {
					continue
				}
				out = append(out, sig)
			}
			if offset > 0 {
				if offset >= len(out) {
					out = nil
				} else {
					out = out[offset:]
				}
			}
			if len(out) > limit {
				out = out[:limit]
			}
			httpx.Respond(w, r, http.StatusOK, out,
				&httpx.Meta{Count: len(out), Limit: limit, Offset: offset}, a.degraded())
			return
		}
	}
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	out, err := a.d.Store.ListSignals(r.Context(), store.Query{
		Ticker: domain.NormalizeTicker(r.URL.Query().Get("ticker")),
		Status: status, Strategy: r.URL.Query().Get("strategy"),
		Limit: limit, Offset: intParam(r, "offset", 0), From: timeParam(r, "from"),
	})
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: limit}, a.degraded())
}

func (a *API) getSignal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.d.Signals != nil {
		if s, ok := a.d.Signals.Get(id); ok {
			httpx.Respond(w, r, http.StatusOK, s, nil, a.degraded())
			return
		}
	}
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrNotFound)
		return
	}
	s, err := a.d.Store.GetSignal(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, s, nil, a.degraded())
}

// Provenance is the complete causal record behind one signal.
//
// This endpoint is the platform's core promise made concrete (requirement §50):
// given a signal id it returns the feature snapshot and its hash, the regime,
// the model version and artifact digest, every rule that fired, every risk
// check with its verdict, and the realised outcome once known.
type Provenance struct {
	Signal       domain.Signal              `json:"signal"`
	Snapshot     *domain.FeatureSnapshot    `json:"feature_snapshot,omitempty"`
	SnapshotErr  string                     `json:"feature_snapshot_error,omitempty"`
	Prediction   *domain.Prediction         `json:"prediction,omitempty"`
	Risk         *domain.RiskAssessment     `json:"risk_assessment,omitempty"`
	Regime       *domain.MarketRegime       `json:"regime,omitempty"`
	Model        *domain.ModelVersion       `json:"model_version,omitempty"`
	Strategy     *domain.Strategy           `json:"strategy,omitempty"`
	Outcomes     []domain.PredictionOutcome `json:"outcomes,omitempty"`
	Reproducible bool                       `json:"reproducible"`
	Missing      []string                   `json:"missing,omitempty"`
	Explanation  string                     `json:"explanation,omitempty"`
	Note         string                     `json:"note"`
}

func (a *API) provenance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var sig domain.Signal
	var found bool
	if a.d.Signals != nil {
		sig, found = a.d.Signals.Get(id)
	}
	if !found && a.d.Store != nil {
		s, err := a.d.Store.GetSignal(r.Context(), id)
		if err != nil {
			httpx.Fail(w, r, mapStoreError(err))
			return
		}
		sig, found = s, true
	}
	if !found {
		httpx.Fail(w, r, httpx.ErrNotFound)
		return
	}

	p := Provenance{Signal: sig, Reproducible: true, Explanation: sig.Explanation}
	p.Note = "This record contains every input that produced the signal. Recomputing the prediction " +
		"from the feature snapshot with the named model artifact reproduces the stored distribution exactly."

	if a.d.Store != nil {
		if snap, err := a.d.Store.GetSnapshot(r.Context(), sig.Ticker, sig.FeatureHash); err == nil {
			p.Snapshot = &snap
		} else {
			p.SnapshotErr = err.Error()
			p.Reproducible = false
			p.Missing = append(p.Missing, "feature_snapshot")
		}
		if sig.PredictionID != "" {
			if pred, err := a.d.Store.GetPrediction(r.Context(), sig.PredictionID); err == nil {
				p.Prediction = &pred
			} else {
				p.Missing = append(p.Missing, "prediction")
			}
		}
		if sig.RiskAssessmentID != "" {
			if ra, err := a.d.Store.GetAssessment(r.Context(), sig.RiskAssessmentID); err == nil {
				p.Risk = &ra
			} else {
				p.Reproducible = false
				p.Missing = append(p.Missing, "risk_assessment")
			}
		}
		if outs, err := a.d.Store.ListOutcomes(r.Context(), store.Query{Ticker: sig.Ticker, Limit: 50}); err == nil {
			for _, o := range outs {
				if o.PredictionID == sig.PredictionID {
					p.Outcomes = append(p.Outcomes, o)
				}
			}
		}
		if models, err := a.d.Store.ListModelVersions(r.Context()); err == nil {
			for _, m := range models {
				if m.Version == sig.ModelVersion {
					mv := m
					p.Model = &mv
					break
				}
			}
		}
	} else {
		p.Reproducible = false
		p.Missing = append(p.Missing, "persistent store not configured in this mode")
	}
	if a.d.Strategies != nil {
		if s, ok := a.d.Strategies.Get(sig.StrategyID); ok {
			p.Strategy = &s
		}
	}
	rg := a.d.State.Regime()
	p.Regime = &rg
	if !p.Reproducible {
		p.Note = "This record is incomplete: the inputs listed under `missing` could not be retrieved, " +
			"so the decision cannot be fully reproduced from stored state."
	}
	httpx.Respond(w, r, http.StatusOK, p, nil, a.degraded())
}

func (a *API) listPredictions(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	if t := r.URL.Query().Get("ticker"); t != "" {
		preds := a.d.State.Predictions()
		if p, ok := preds[domain.NormalizeTicker(t)]; ok {
			httpx.Respond(w, r, http.StatusOK, []domain.Prediction{p}, &httpx.Meta{Count: 1}, a.degraded())
			return
		}
	}
	if a.d.Store != nil {
		out, err := a.d.Store.ListPredictions(r.Context(), store.Query{
			Ticker: domain.NormalizeTicker(r.URL.Query().Get("ticker")),
			Limit:  limit, From: timeParam(r, "from"),
		})
		if err == nil {
			httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: limit}, a.degraded())
			return
		}
	}
	preds := a.d.State.Predictions()
	out := make([]domain.Prediction, 0, len(preds))
	for _, p := range preds {
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: limit}, a.degraded())
}

func (a *API) getPrediction(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	p, err := a.d.Store.GetPrediction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, p, nil, a.degraded())
}

func (a *API) listAlerts(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	limit := intParam(r, "limit", 100)
	out, err := a.d.Store.ListAlerts(r.Context(), store.Query{
		Ticker: domain.NormalizeTicker(r.URL.Query().Get("ticker")),
		Limit:  limit, From: timeParam(r, "from"), Offset: intParam(r, "offset", 0),
	})
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: limit}, a.degraded())
}

func (a *API) riskFor(w http.ResponseWriter, r *http.Request) {
	t := domain.NormalizeTicker(chi.URLParam(r, "ticker"))
	risks := a.d.State.Risks()
	ra, ok := risks[t]
	if !ok {
		httpx.Fail(w, r, httpx.ErrNotFound.WithDetail("no risk assessment recorded for "+string(t)))
		return
	}
	httpx.Respond(w, r, http.StatusOK, ra, httpx.At(ra.CreatedAt), a.degraded())
}

// --- Models ----------------------------------------------------------------

func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	type modelInfo struct {
		ID       string   `json:"id"`
		Version  string   `json:"version"`
		Family   string   `json:"family"`
		Horizon  string   `json:"horizon"`
		Features []string `json:"features"`
		Digest   string   `json:"artifact_sha"`
	}
	var out []modelInfo
	if a.d.Models != nil {
		for _, m := range a.d.Models.All() {
			art := m.Artifact()
			out = append(out, modelInfo{
				ID: art.ModelID, Version: art.Version, Family: string(art.Family),
				Horizon: string(art.Horizon), Features: art.Features, Digest: art.Digest,
			})
		}
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
}

func (a *API) modelHealth(w http.ResponseWriter, r *http.Request) {
	h, d := a.d.State.ModelHealth()
	resp := map[string]any{"health": h, "drift": d}
	if a.d.Evaluator != nil {
		outcomes := a.d.Evaluator.Resolved()
		resp["overall"] = evaluation.Evaluate(outcomes, "rolling", 10)
		resp["by_regime"] = evaluation.ByRegime(outcomes, 10, 20)
		resp["veto_cost"] = evaluation.MeasureVetoCost(outcomes)
		resp["pending"] = a.d.Evaluator.PendingCount()
	}
	httpx.Respond(w, r, http.StatusOK, resp, httpx.At(h.AsOf), a.degraded())
}

func (a *API) evaluations(w http.ResponseWriter, r *http.Request) {
	if a.d.Evaluator == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	outcomes := a.d.Evaluator.Resolved()
	limit := intParam(r, "limit", 500)
	if len(outcomes) > limit {
		outcomes = outcomes[len(outcomes)-limit:]
	}
	httpx.Respond(w, r, http.StatusOK, map[string]any{
		"outcomes":    outcomes,
		"evaluation":  evaluation.Evaluate(outcomes, "rolling", 10),
		"calibration": evaluation.Calibrate(outcomes, domain.OutcomeUp, 10),
	}, &httpx.Meta{Count: len(outcomes)}, a.degraded())
}

func (a *API) listStrategies(w http.ResponseWriter, r *http.Request) {
	if a.d.Strategies == nil {
		httpx.Respond(w, r, http.StatusOK, []domain.Strategy{}, nil, a.degraded())
		return
	}
	out := a.d.Strategies.All()
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
}

// --- Paper trading ---------------------------------------------------------

func (a *API) portfolio(w http.ResponseWriter, r *http.Request) {
	if a.d.Broker == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	p := a.d.Broker.Portfolio()
	httpx.Respond(w, r, http.StatusOK, map[string]any{
		"portfolio":    p,
		"equity_curve": a.d.Broker.EquityCurve(),
		"trades":       a.d.Broker.Trades(),
	}, httpx.At(p.AsOf), a.degraded())
}

func (a *API) positions(w http.ResponseWriter, r *http.Request) {
	if a.d.Broker == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	out := a.d.Broker.Positions()
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
}

func (a *API) orders(w http.ResponseWriter, r *http.Request) {
	if a.d.Broker == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	out := a.d.Broker.Orders()
	limit := intParam(r, "limit", 100)
	if len(out) > limit {
		out = out[:limit]
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out), Limit: limit}, a.degraded())
}

type orderRequest struct {
	IdempotencyKey string  `json:"idempotency_key"`
	Ticker         string  `json:"ticker"`
	Side           string  `json:"side"`
	Type           string  `json:"type"`
	Quantity       float64 `json:"quantity"`
	LimitPrice     float64 `json:"limit_price,omitempty"`
	StopPrice      float64 `json:"stop_price,omitempty"`
}

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	if a.d.Broker == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	var req orderRequest
	if err := httpx.DecodeJSON(w, r, 8192, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	t := domain.NormalizeTicker(req.Ticker)
	stock, ok := a.d.Universe.Get(t)
	if !ok {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail("unknown ticker "+req.Ticker))
		return
	}
	side := domain.OrderSide(strings.ToUpper(req.Side))
	if side != domain.OrderBuy && side != domain.OrderSell {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(`side must be "BUY" or "SELL"`))
		return
	}
	if req.Quantity <= 0 {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail("quantity must be positive"))
		return
	}
	if req.IdempotencyKey == "" {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(
			"idempotency_key is required: order submission must be safe to retry"))
		return
	}
	ord, err := a.d.Broker.Submit(paper.OrderRequest{
		IdempotencyKey: "api:" + req.IdempotencyKey,
		Ticker:         t,
		Side:           side,
		Type:           domain.OrderType(orDefault(strings.ToUpper(req.Type), string(domain.OrderMarket))),
		Quantity:       req.Quantity,
		LimitPrice:     req.LimitPrice,
		StopPrice:      req.StopPrice,
		Sector:         stock.Sector,
		Now:            a.d.Clock.Now(),
	})
	if err != nil {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(err.Error()))
		return
	}
	if a.d.Store != nil {
		_, _ = a.d.Store.SaveOrder(r.Context(), ord)
	}
	httpx.Respond(w, r, http.StatusAccepted, ord, nil, a.degraded())
}

// --- Backtests -------------------------------------------------------------

type backtestRequest struct {
	Name         string   `json:"name"`
	StrategyID   string   `json:"strategy_id"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Interval     string   `json:"interval,omitempty"`
	Seed         int64    `json:"seed,omitempty"`
	StartingCash float64  `json:"starting_cash,omitempty"`
	Tickers      []string `json:"tickers,omitempty"`
}

func (a *API) createBacktest(w http.ResponseWriter, r *http.Request) {
	if a.d.Backtests == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable.WithDetail("backtesting is not enabled in this deployment"))
		return
	}
	var req backtestRequest
	if err := httpx.DecodeJSON(w, r, 16384, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail("start must be an RFC3339 timestamp"))
		return
	}
	end, err := time.Parse(time.RFC3339, req.End)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail("end must be an RFC3339 timestamp"))
		return
	}
	if !end.After(start) {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail("end must be after start"))
		return
	}
	if a.d.Strategies != nil {
		if _, ok := a.d.Strategies.Get(req.StrategyID); !ok {
			httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail("unknown strategy "+req.StrategyID))
			return
		}
	}
	tickers := make([]domain.Ticker, 0, len(req.Tickers))
	for _, t := range req.Tickers {
		tickers = append(tickers, domain.NormalizeTicker(t))
	}
	run, err := a.d.Backtests.Submit(r.Context(), backtest.Request{
		Name: orDefault(req.Name, "adhoc"), StrategyID: req.StrategyID,
		Start: start, End: end, Seed: req.Seed,
		Interval:     domain.Interval(orDefault(req.Interval, string(domain.Interval1m))),
		StartingCash: req.StartingCash,
	}, tickers)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(err.Error()))
		return
	}
	httpx.Respond(w, r, http.StatusAccepted, run, nil, a.degraded())
}

func (a *API) listBacktests(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	out, err := a.d.Store.ListRuns(r.Context(), intParam(r, "limit", 50))
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
}

func (a *API) getBacktest(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	run, err := a.d.Store.GetRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, run, nil, a.degraded())
}

// --- Brief and audit -------------------------------------------------------

func (a *API) brief(w http.ResponseWriter, r *http.Request) {
	if a.d.Brief == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	ev, ct := a.d.State.Evidence()
	h, d := a.d.State.ModelHealth()
	var outcomes []domain.PredictionOutcome
	if a.d.Evaluator != nil {
		outcomes = a.d.Evaluator.Resolved()
	}
	b := a.d.Brief.Generate(r.Context(), brief.Input{
		Now: a.d.Clock.Now(), Regime: a.d.State.Regime(),
		Relationship: a.d.State.Relationship(), Snapshots: a.d.State.Snapshots(),
		Universe: a.d.Universe.Get, Scores: a.d.State.Scores(),
		Predictions: a.d.State.Predictions(), Opportunities: a.d.State.Opportunities(),
		Risks: a.d.State.Risks(), Evidence: ev, Counter: ct,
		Events: a.d.State.AllEvents(), News: a.d.State.AllNews(50),
		Outcomes: outcomes, Health: h, Drift: d, TopN: intParam(r, "top", 10),
	})
	httpx.Respond(w, r, http.StatusOK, b, httpx.At(b.GeneratedAt), a.degraded())
}

func (a *API) audit(w http.ResponseWriter, r *http.Request) {
	if a.d.Store == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable)
		return
	}
	out, err := a.d.Store.ListAudit(r.Context(), intParam(r, "limit", 200))
	if err != nil {
		httpx.Fail(w, r, mapStoreError(err))
		return
	}
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
}

// --- helpers ---------------------------------------------------------------

func mapStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return httpx.ErrNotFound
	case errors.Is(err, store.ErrUnavailable):
		return httpx.ErrUnavailable.WithDetail(err.Error())
	default:
		return httpx.ErrInternal.WithDetail(err.Error())
	}
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	if n > 5000 {
		return 5000
	}
	return n
}

func timeParam(r *http.Request, name string) time.Time {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Ctx is a convenience for handlers that need a bounded context.
func Ctx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
