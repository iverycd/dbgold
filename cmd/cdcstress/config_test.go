package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func baseTestConfig() Config {
	return Config{
		API:     APIConfig{BaseURL: "http://127.0.0.1:8080", SourceConnectionID: 1, TargetConnectionID: 2},
		MySQL:   DatabaseConfig{Host: "127.0.0.1", Port: 3306, User: "test", Database: defaultNamespace},
		GaussDB: DatabaseConfig{Host: "127.0.0.1", Port: 5432, User: "test", Database: "postgres", Schema: defaultNamespace},
	}
}

func TestProfilePresets(t *testing.T) {
	tests := []struct {
		name   string
		tables int
		rows   int64
	}{{"small", 100, 1_000_000}, {"medium", 1000, 10_000_000}, {"large", 3000, 30_000_000}}
	for _, test := range tests {
		cfg := baseTestConfig()
		cfg.Profile.Name = test.name
		cfg.applyDefaults()
		if cfg.Profile.TableCount != test.tables || cfg.Profile.TotalRows != test.rows {
			t.Fatalf("%s=%d/%d", test.name, cfg.Profile.TableCount, cfg.Profile.TotalRows)
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
	}
}

func TestNamespaceSafety(t *testing.T) {
	for _, name := range []string{defaultNamespace, defaultNamespace + "_large", defaultNamespace + "_2"} {
		if !safeNamespace(name) {
			t.Fatalf("expected safe: %s", name)
		}
	}
	for _, name := range []string{"production", defaultNamespace + "-x", "DBGold_cdc_stress", defaultNamespace + ".x"} {
		if safeNamespace(name) {
			t.Fatalf("expected unsafe: %s", name)
		}
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"2m30s"`)); err != nil {
		t.Fatal(err)
	}
	if d.Duration != 150*time.Second {
		t.Fatalf("duration=%s", d.Duration)
	}
}

func TestResolvedPoolDefaultsAndOverrides(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Profile.Workers = 4
	cfg.Workload.Workers = 8
	got := cfg.resolvedPool()
	if got.MaxOpenConns != 12 || got.MaxIdleConns != 12 || got.ConnMaxLifetime != 30*time.Minute || got.ConnMaxIdleTime != 5*time.Minute {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	cfg.Pool = &PoolConfig{MaxOpenConns: 20, MaxIdleConns: 10, ConnMaxLifetime: Duration{time.Hour}, ConnMaxIdleTime: Duration{10 * time.Minute}}
	got = cfg.resolvedPool()
	if got.MaxOpenConns != 20 || got.MaxIdleConns != 10 || got.ConnMaxLifetime != time.Hour || got.ConnMaxIdleTime != 10*time.Minute {
		t.Fatalf("unexpected overrides: %+v", got)
	}
}

func TestOptionalPoolDoesNotChangeLegacyConfigHash(t *testing.T) {
	cfg := baseTestConfig()
	cfg.applyDefaults()
	before := cfg.hash()
	cfg.Pool = nil
	if after := cfg.hash(); after != before {
		t.Fatalf("nil optional pool changed hash: %s != %s", after, before)
	}
}

func TestConfigureDBPoolSetsMaximumConnections(t *testing.T) {
	db, err := sql.Open("mysql", "test:test@tcp(127.0.0.1:1)/test")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	configureDBPool(db, resolvedPoolConfig{MaxOpenConns: 17, MaxIdleConns: 17, ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Minute})
	if got := db.Stats().MaxOpenConnections; got != 17 {
		t.Fatalf("MaxOpenConnections=%d, want 17", got)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig("cdcstress.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool == nil || cfg.resolvedPool().MaxOpenConns != 12 {
		t.Fatalf("example pool was not loaded: %+v", cfg.Pool)
	}
}

func TestCleanupRequiresMatchingConfirmationBeforeDatabaseAccess(t *testing.T) {
	cfg := baseTestConfig()
	cfg.applyDefaults()
	state := RunState{RunID: "run-safe", ConfigHash: cfg.hash()}
	if err := cleanup(context.Background(), cfg, state, "wrong"); err == nil {
		t.Fatal("expected safety rejection")
	}
	state.ActiveJobID = "active-job"
	if err := cleanup(context.Background(), cfg, state, state.RunID); err == nil {
		t.Fatal("expected active job rejection")
	}
}
