package config

import (
	"strings"
	"testing"
)

func TestExecutionIsOffByDefault(t *testing.T) {
	c := Default()
	if c.Execution.Enabled {
		t.Fatal("execution must be opt-in")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestExecutionValidation(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Auth.Enabled = true
		c.Execution.Enabled = true
		c.Execution.Allowlist = []string{"AAPL"}
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a minimal enabled config must validate: %v", err)
	}
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no allowlist":        {func(c *Config) { c.Execution.Allowlist = nil }, "execution.allowlist"},
		"live venue":          {func(c *Config) { c.Execution.Venue = "alpaca-live" }, "rule G-8"},
		"auto-propose on sim": {func(c *Config) { c.Market.Provider = "sim"; c.Execution.AutoPropose = true }, "live market feed"},
		"no auth":             {func(c *Config) { c.Auth.Enabled = false }, "requires auth.enabled"},
		"weight out of range": {func(c *Config) { c.Execution.MaxPositionWeight = 2 }, "max_position_weight"},
		"loss limit of 100%":  {func(c *Config) { c.Execution.MaxDailyLossPct = 1 }, "max_daily_loss_pct"},
		"zero order notional": {func(c *Config) { c.Execution.MaxOrderNotional = 0 }, "max_order_notional"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMarketProviderValidation(t *testing.T) {
	c := Default()
	c.Market.Provider = "alpaca"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "at least 3s") {
		t.Fatalf("a 1s poll against alpaca must be refused, got %v", err)
	}
	c.Market.TickInterval = 5_000_000_000
	if err := c.Validate(); err != nil {
		t.Fatalf("alpaca at 5s must validate: %v", err)
	}
	c.Market.Provider = "bloomberg"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "sim|replay|alpaca") {
		t.Fatalf("an unknown provider must be refused, got %v", err)
	}
	ok := Default()
	ok.Auth.Enabled = true
	ok.Market.Provider, ok.Market.TickInterval = "alpaca", 5_000_000_000
	ok.Execution.Enabled, ok.Execution.Allowlist, ok.Execution.AutoPropose = true, []string{"AAPL"}, true
	if err := ok.Validate(); err != nil {
		t.Fatalf("auto_propose on a live feed must validate: %v", err)
	}
}
