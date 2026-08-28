// Command portfolio-service marks the simulated book and persists portfolio
// state.
//
// Marking is cheap and frequent; risk re-assessment is expensive and less
// frequent. Running them in one loop would force a single cadence that is wrong
// for one of them, so they are separate services with separate intervals.
package main

import (
	"context"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "portfolio-service",
		Roles:    []app.Role{},
		WithHTTP: true,
		Extra: func(ctx context.Context, a *app.App) error {
			return a.RunPortfolio(ctx)
		},
	})
}
