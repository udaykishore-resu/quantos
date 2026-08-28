// Command backtest-service executes queued backtest runs.
//
// It is a separate service so a parameter sweep can never contend with the live
// decision path for CPU: a backtest is minutes of single-threaded work, and the
// live path has a hundred-millisecond budget.
package main

import (
	"context"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "backtest-service",
		Roles:    []app.Role{},
		WithHTTP: true,
		Extra: func(ctx context.Context, a *app.App) error {
			return a.RunBacktestWorker(ctx)
		},
	})
}
