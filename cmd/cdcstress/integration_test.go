//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestCDCStressIntegration is deliberately gated twice because it creates and
// drops databases, schemas, and tables on real servers.
func TestCDCStressIntegration(t *testing.T) {
	if os.Getenv("CDCSTRESS_INTEGRATION") != "1" {
		t.Skip("set CDCSTRESS_INTEGRATION=1 and CDCSTRESS_CONFIG to run destructive integration coverage")
	}
	path := os.Getenv("CDCSTRESS_CONFIG")
	if path == "" {
		t.Fatal("CDCSTRESS_CONFIG is required")
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile.TableCount > 20 || cfg.Profile.TotalRows > 100_000 {
		t.Fatal("integration test config is too large; use the CLI for scale tiers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	state := newRunState(cfg, makeRunID())
	if err = precheck(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err = prepare(ctx, cfg, &state, false); err != nil {
		t.Fatal(err)
	}
	if err = executeRun(ctx, cfg, &state, "both", false, 0, ""); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CDCSTRESS_INTEGRATION_CLEANUP") == "1" {
		if err = cleanup(ctx, cfg, state, state.RunID); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("run %s passed; data retained because CDCSTRESS_INTEGRATION_CLEANUP is not 1", state.RunID)
	}
}
