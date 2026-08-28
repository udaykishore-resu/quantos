// Command api-gateway serves the REST API and the Server-Sent Events stream.
//
// It holds no decision logic. Its job is authentication, authorisation, rate
// limiting, correlation, the uniform response envelope and the read model —
// everything that must be identical across every endpoint.
package main

import (
	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "api-gateway",
		Roles:    []app.Role{app.RoleAPI},
		WithHTTP: true,
	})
}
