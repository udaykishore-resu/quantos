package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/udaykishoreresu/quantos/internal/auth"
	"github.com/udaykishoreresu/quantos/internal/domain"
	"github.com/udaykishoreresu/quantos/internal/execution"
	"github.com/udaykishoreresu/quantos/internal/httpx"
)

// The execution endpoints expose the approval desk (internal/execution).
//
//	GET  /execution/status                  venue, account, limits, kill switch
//	GET  /execution/positions               holdings at the venue
//	GET  /execution/tickets?status=&limit=  tickets, newest first
//	GET  /execution/tickets/{id}            one ticket with its audit trail
//	POST /execution/tickets                 propose a manual trade
//	POST /execution/tickets/{id}/approve    approve (re-enter password)
//	POST /execution/tickets/{id}/reject     reject
//	POST /execution/halt                    kill switch (no password: stopping must be easy)
//	POST /execution/resume                  lift it (re-enter password: starting must not be)
//
// Reads need portfolio:read; everything else needs orders:approve.

func (a *API) mountExecution(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireScope(auth.ScopeReadPortfolio))
		r.Get("/execution/status", a.execStatus)
		r.Get("/execution/positions", a.execPositions)
		r.Get("/execution/tickets", a.execList)
		r.Get("/execution/tickets/{id}", a.execGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireScope(auth.ScopeApproveOrders))
		r.Post("/execution/tickets", a.execPropose)
		r.Post("/execution/tickets/{id}/approve", a.execApprove)
		r.Post("/execution/tickets/{id}/reject", a.execReject)
		r.Post("/execution/halt", a.execHalt)
		r.Post("/execution/resume", a.execResume)
	})
}

func (a *API) desk(w http.ResponseWriter, r *http.Request) *execution.Desk {
	if a.d.Desk == nil {
		httpx.Fail(w, r, httpx.ErrUnavailable.WithDetail(
			"the execution desk is not running: set execution.enabled and the venue's paper API keys"))
		return nil
	}
	return a.d.Desk
}

type execStatusResponse struct {
	Venue       string             `json:"venue"`
	Paper       bool               `json:"paper"`
	Halt        execution.Halt     `json:"halt"`
	Account     *execution.Account `json:"account,omitempty"`
	AccountErr  string             `json:"account_error,omitempty"`
	Limits      execution.Limits   `json:"limits"`
	Pending     int                `json:"pending"`
	Working     int                `json:"working"`
	AutoPropose bool               `json:"auto_propose"`
}

func (a *API) execStatus(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	resp := execStatusResponse{
		Venue: d.Venue().Name(), Paper: d.Venue().Paper(), Halt: d.HaltState(),
		Limits: d.Limits(), AutoPropose: a.d.AutoPropose,
	}
	if acct, err := d.Venue().Account(r.Context()); err != nil {
		resp.AccountErr = err.Error()
	} else {
		resp.Account = &acct
	}
	for _, t := range d.List(r.Context(), "", 0) {
		switch {
		case t.Status == execution.TicketProposed:
			resp.Pending++
		case t.Status.Open():
			resp.Working++
		}
	}
	httpx.Respond(w, r, http.StatusOK, resp, nil, a.degraded())
}

func (a *API) execPositions(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	pos, err := d.Venue().Positions(r.Context())
	if err != nil {
		httpx.Fail(w, r, httpx.ErrUnavailable.WithDetail("venue: "+err.Error()))
		return
	}
	httpx.Respond(w, r, http.StatusOK, pos, &httpx.Meta{Count: len(pos)}, a.degraded())
}

func (a *API) execList(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	status := execution.TicketStatus(strings.ToUpper(r.URL.Query().Get("status")))
	out := d.List(r.Context(), status, intParam(r, "limit", 200))
	httpx.Respond(w, r, http.StatusOK, out, &httpx.Meta{Count: len(out)}, a.degraded())
}

func (a *API) execGet(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	t, err := d.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		execFail(w, r, err)
		return
	}
	httpx.Respond(w, r, http.StatusOK, t, nil, a.degraded())
}

type execProposeRequest struct {
	Ticker     string  `json:"ticker"`
	Side       string  `json:"side"`
	Quantity   float64 `json:"quantity"`
	LimitPrice float64 `json:"limit_price,omitempty"`
	Rationale  string  `json:"rationale"`
}

func (a *API) execPropose(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil || a.d.Proposer == nil {
		if d != nil {
			httpx.Fail(w, r, httpx.ErrUnavailable)
		}
		return
	}
	var req execProposeRequest
	if err := httpx.DecodeJSON(w, r, 8192, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	side := domain.OrderSide(strings.ToUpper(req.Side))
	if side != domain.OrderBuy && side != domain.OrderSell {
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(`side must be "BUY" or "SELL"`))
		return
	}
	prop, err := a.d.Proposer.Manual(r.Context(), domain.Ticker(req.Ticker), side, req.Quantity, req.LimitPrice, req.Rationale)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrUnavailable.WithDetail(err.Error()))
		return
	}
	t, err := d.Propose(r.Context(), prop)
	if err != nil {
		execFail(w, r, err)
		return
	}
	httpx.Respond(w, r, http.StatusCreated, t, nil, a.degraded())
}

type execStepUp struct {
	// Password is the principal's own password, re-entered. A bearer token
	// proves a session exists; this proves a person is at it.
	Password string `json:"password"`
	Reason   string `json:"reason,omitempty"`
}

// stepUp re-verifies the caller and returns their subject.
func (a *API) stepUp(w http.ResponseWriter, r *http.Request, body *execStepUp) (string, bool) {
	if err := httpx.DecodeJSON(w, r, 4096, body); err != nil {
		httpx.Fail(w, r, err)
		return "", false
	}
	p := httpx.PrincipalFrom(r.Context())
	if a.d.Auth == nil || p.Subject == "" || !a.d.Auth.VerifyPassword(p.Subject, body.Password) {
		httpx.Fail(w, r, httpx.ErrUnauthorized.WithDetail(
			"re-enter your password to confirm; only locally-configured principals can approve orders"))
		return "", false
	}
	return p.Subject, true
}

func (a *API) execApprove(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	var body execStepUp
	who, ok := a.stepUp(w, r, &body)
	if !ok {
		return
	}
	// The approval must not be abandoned half-way because the browser went
	// away: the claim is already written, and the desk must finish the
	// submission or record why it could not.
	ctx := context.WithoutCancel(r.Context())
	t, err := d.Approve(ctx, chi.URLParam(r, "id"), who)
	if err != nil {
		execFail(w, r, err, t)
		return
	}
	httpx.Respond(w, r, http.StatusOK, t, nil, a.degraded())
}

func (a *API) execReject(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, 4096, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}
	t, err := d.Reject(r.Context(), chi.URLParam(r, "id"), httpx.PrincipalFrom(r.Context()).Subject, body.Reason)
	if err != nil {
		execFail(w, r, err, t)
		return
	}
	httpx.Respond(w, r, http.StatusOK, t, nil, a.degraded())
}

func (a *API) execHalt(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// An empty or malformed body still halts: the kill switch must work when
	// everything else is going wrong.
	_ = httpx.DecodeJSON(w, r, 4096, &body)
	err := d.Halt(context.WithoutCancel(r.Context()), httpx.PrincipalFrom(r.Context()).Subject, body.Reason)
	resp := map[string]any{"halt": d.HaltState()}
	if err != nil {
		resp["error"] = err.Error()
	}
	httpx.Respond(w, r, http.StatusOK, resp, nil, a.degraded())
}

func (a *API) execResume(w http.ResponseWriter, r *http.Request) {
	d := a.desk(w, r)
	if d == nil {
		return
	}
	var body execStepUp
	who, ok := a.stepUp(w, r, &body)
	if !ok {
		return
	}
	if err := d.Resume(r.Context(), who); err != nil {
		httpx.Fail(w, r, httpx.ErrUnavailable.WithDetail(err.Error()))
		return
	}
	httpx.Respond(w, r, http.StatusOK, map[string]any{"halt": d.HaltState()}, nil, a.degraded())
}

// execFail maps desk errors to responses. A blocked or expired approval is a
// recorded decision, not a server failure, so it returns the ticket with 409
// and the reason in the body.
func execFail(w http.ResponseWriter, r *http.Request, err error, t ...execution.Ticket) {
	conflict := httpx.APIError{Status: http.StatusConflict, Code: "conflict"}
	switch {
	case errors.Is(err, execution.ErrNotFound):
		httpx.Fail(w, r, httpx.ErrNotFound)
	case errors.Is(err, execution.ErrBlocked), errors.Is(err, execution.ErrExpired):
		if len(t) > 0 && t[0].ID != "" {
			httpx.Respond(w, r, http.StatusConflict, t[0], nil, nil)
			return
		}
		conflict.Message = err.Error()
		httpx.Fail(w, r, conflict)
	case errors.Is(err, execution.ErrNotProposed):
		conflict.Message = "the ticket has already been decided"
		httpx.Fail(w, r, conflict)
	case errors.Is(err, execution.ErrHalted), errors.Is(err, execution.ErrDuplicateSignal):
		conflict.Message = err.Error()
		httpx.Fail(w, r, conflict)
	case errors.Is(err, execution.ErrSkipped):
		httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(err.Error()))
	default:
		var apiErr *execution.APIError
		if errors.As(err, &apiErr) {
			httpx.Fail(w, r, httpx.APIError{Status: http.StatusBadGateway, Code: "venue_error",
				Message: "the venue refused the request", Detail: apiErr.Error()})
			return
		}
		if strings.HasPrefix(err.Error(), "execution: proposal") {
			httpx.Fail(w, r, httpx.ErrBadRequest.WithDetail(err.Error()))
			return
		}
		httpx.Fail(w, r, httpx.ErrInternal.WithDetail(err.Error()))
	}
}
