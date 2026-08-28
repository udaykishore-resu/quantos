// Command evaluation-service scores elapsed predictions and detects drift.
//
// It scores the predictions the risk engine blocked as well as the ones it
// allowed, which is what makes the veto's cost measurable rather than a matter
// of faith (ADR-009).
package main

import (
	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "evaluation-service",
		Roles:    []app.Role{app.RoleEvaluation},
		WithHTTP: true,
	})
}
