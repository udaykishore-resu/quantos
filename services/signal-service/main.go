// Command signal-service runs the decision path: features, regime, rules,
// prediction, risk, scoring, opportunity and signal generation.
//
// The stages are co-located deliberately (ADR-006): they are a single causal
// chain over one symbol's state, and splitting them across network hops would
// add latency and failure modes without buying independent scaling, because
// they scale on the same axis — events per symbol.
package main

import (
	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:    "signal-service",
		Roles:      []app.Role{app.RoleEngine},
		WithHTTP:   true,
		TradePaper: true,
		Explain:    true,
	})
}
