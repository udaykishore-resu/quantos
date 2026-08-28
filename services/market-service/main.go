// Command market-service ingests market data, validates it and publishes the
// canonical market topics.
//
// It owns the staleness gate: when a symbol's data ages out it publishes to
// market.stale, and the decision path suspends new signals for that symbol
// (governance rule G-5).
package main

import (
	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "market-service",
		Roles:    []app.Role{app.RoleIngest},
		WithHTTP: true,
	})
}
