// Command alert-service delivers alerts to external sinks.
//
// Delivery is idempotent on the alert's dedup key, so a redelivered event
// cannot notify anyone twice (requirement §36). Generation happens in the
// decision path; this service is only responsible for getting an alert out.
package main

import (
	"context"
	"os"
	"time"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/cli"
)

func main() {
	cli.Main(cli.Spec{
		Service:  "alert-service",
		Roles:    []app.Role{},
		WithHTTP: true,
		Extra: func(ctx context.Context, a *app.App) error {
			sinks := []app.AlertSink{app.LogSink{App: a}}
			if url := os.Getenv("QUANTOS_ALERT_WEBHOOK"); url != "" {
				sinks = append(sinks, app.WebhookSink{URL: url, Timeout: 5 * time.Second})
				a.Log.Info("alert webhook configured")
			}
			return a.RunAlertDelivery(ctx, sinks...)
		},
	})
}
