// Command news-service runs the news pipeline.
//
// Deterministic classification runs first and always; the language model may
// only refine the result into the same typed schema, and its influence is
// capped (ADR-005). The raw article text never reaches the decision path.
package main

import (
	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "news-service",
		Roles:    []app.Role{app.RoleNews},
		WithHTTP: true,
	})
}
