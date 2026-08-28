// Package cli holds the shared start-up path for every QuantOS binary.
//
// Service main() functions are deliberately thin (ADR-011): they name their
// roles and call Main. Flag parsing, configuration loading, signal handling and
// graceful shutdown live here once, so no service can quietly get them wrong.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/udaykishoreresu/quantos/internal/app"
	"github.com/udaykishoreresu/quantos/internal/config"
	"github.com/udaykishoreresu/quantos/internal/marketdata"
)

// Spec describes one binary.
type Spec struct {
	Service string
	Roles   []app.Role
	// WithHTTP builds the API server. Every service serves /healthz and
	// /metrics even when it exposes no API, because a service you cannot probe
	// is a service you cannot operate.
	WithHTTP bool
	// TradePaper enables paper order submission from generated signals.
	TradePaper bool
	// Explain enables AI explanations for generated signals.
	Explain bool
	// Extra lets a binary run additional work alongside its roles.
	Extra func(ctx context.Context, a *app.App) error
}

// Main is the entry point every service delegates to.
func Main(spec Spec) {
	var (
		configPath = flag.String("config", envOr("QUANTOS_CONFIG", "config/quantos.yaml"), "path to the configuration file")
		addr       = flag.String("addr", "", "HTTP listen address (overrides configuration)")
		mode       = flag.String("mode", "", "runtime mode: embedded|compose|cluster")
		printCfg   = flag.Bool("print-config", false, "print the effective configuration and exit")
		version    = flag.Bool("version", false, "print the version and exit")
		scenario   = flag.String("scenario", envOr("QUANTOS_SCENARIO", ""),
			"apply a simulated market condition ("+strings.Join(marketdata.ScenarioNames(), "|")+"); "+
				"a quiet random walk produces no directional edge, so the signal path stays idle without one")
	)
	flag.Parse()

	if *version {
		fmt.Printf("quantos %s (%s)\n", buildVersion, buildCommit)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("configuration: %v", err)
	}
	cfg.Service = spec.Service
	if *addr != "" {
		cfg.HTTP.Addr = *addr
	}
	if *mode != "" {
		cfg.Mode = config.Mode(*mode)
	}
	if buildVersion != "" {
		cfg.Version = buildVersion
	}
	if err := cfg.Validate(); err != nil {
		fatal("configuration: %v", err)
	}
	cfg.Hash = cfg.ComputeHash()

	if *printCfg {
		fmt.Printf("service:      %s\nmode:         %s\nconfig hash:  %s\nsource:       %s\nbus driver:   %s\nstore driver: %s\nprovider:     %s\nllm:          %s\n",
			cfg.Service, cfg.Mode, cfg.Hash, orDefault(cfg.SourcePath, "(defaults)"),
			cfg.Bus.Driver, cfg.Store.Driver, cfg.Market.Provider, cfg.LLM.Provider)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, app.Options{
		WithHTTP:   spec.WithHTTP,
		TradePaper: spec.TradePaper,
		Explain:    spec.Explain,
		Now:        time.Now().UTC(),
	})
	if err != nil {
		fatal("start-up: %v", err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			a.Log.Error("shutdown", "error", err)
		}
	}()

	if *scenario != "" {
		sc, ok := marketdata.NamedScenario(*scenario)
		if !ok {
			fatal("unknown scenario %q; available: %s", *scenario, strings.Join(marketdata.ScenarioNames(), ", "))
		}
		if a.Sim == nil {
			fatal("--scenario requires the simulated market-data provider")
		}
		a.Sim.AddScenario(sc)
		a.Log.Warn("simulated market scenario applied; output is not a neutral market",
			"scenario", sc.Name)
	}

	a.Log.Info("starting",
		"service", cfg.Service, "version", cfg.Version, "mode", cfg.Mode,
		"roles", spec.Roles, "config_hash", cfg.Hash,
		"bus", cfg.Bus.Driver, "store", cfg.Store.Driver, "provider", cfg.Market.Provider)

	errCh := make(chan error, 2)
	if spec.Extra != nil {
		go func() { errCh <- spec.Extra(ctx, a) }()
	}
	go func() { errCh <- a.Run(ctx, spec.Roles...) }()

	select {
	case err := <-errCh:
		if err != nil {
			a.Log.Error("service exited with an error", "error", err)
			stop()
			os.Exit(1)
		}
	case <-ctx.Done():
		a.Log.Info("shutdown signal received; draining")
	}
	// Give in-flight work a bounded window to finish.
	time.Sleep(200 * time.Millisecond)
	a.Log.Info("stopped", "service", cfg.Service)
}

// Build information, set with -ldflags at build time.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
)

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "quantos: "+format+"\n", a...)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
