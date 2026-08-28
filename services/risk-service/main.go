// Command risk-service continuously re-assesses live signals.
//
// Risk is not evaluated once at emission. This service re-runs the invalidation
// conditions on a schedule so that a signal dies from the passage of time, a
// regime change or a fresh risk veto — not only when its own instrument happens
// to print a new bar (ADR-009).
package main

import (
	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "risk-service",
		Roles:    []app.Role{app.RoleSweeper},
		WithHTTP: true,
	})
}
