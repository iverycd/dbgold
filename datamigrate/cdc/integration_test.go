//go:build integration

package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

const (
	integrationSourceAdminDSN = "root:rootpass@tcp(127.0.0.1:13306)/cdc_source?parseTime=true&charset=utf8mb4"
	integrationSourceCDCDSN   = "cdc:cdcpass@tcp(127.0.0.1:13306)/cdc_source?parseTime=true&charset=utf8mb4"
	integrationTargetDSN      = "host=127.0.0.1 port=15432 user=postgres password=postgrespass dbname=cdc_target sslmode=disable"
)

func TestBootstrapCheckpointIntegration(t *testing.T) {
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("set CDC_INTEGRATION=1 and start testdata/docker-compose.yml")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dst := mustOpenIntegrationDB(t, "postgres", integrationTargetDSN)
	defer dst.Close()
	mustExecIntegration(t, dst, `CREATE SCHEMA IF NOT EXISTS cdc_test`)
	mustExecIntegration(t, dst, `DROP TABLE IF EXISTS cdc_test.__dbgold_cdc_checkpoint`)
	mustExecIntegration(t, dst, `CREATE TABLE cdc_test.__dbgold_cdc_checkpoint (
		job_id text PRIMARY KEY, binlog_file text NOT NULL DEFAULT '', binlog_position bigint NOT NULL DEFAULT 4,
		gtid text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT now())`)
	mustExecIntegration(t, dst, `DROP TABLE IF EXISTS cdc_test.bootstrap_failed CASCADE`)
	mustExecIntegration(t, dst, `DROP TABLE IF EXISTS cdc_test.bootstrap_effective CASCADE`)
	mustExecIntegration(t, dst, `DROP TABLE IF EXISTS cdc_test.bootstrap_unrelated CASCADE`)
	mustExecIntegration(t, dst, `DROP TABLE IF EXISTS cdc_test.bootstrap_schema_failed CASCADE`)
	mustExecIntegration(t, dst, `CREATE TABLE cdc_test.bootstrap_failed(id bigint)`)
	mustExecIntegration(t, dst, `CREATE TABLE cdc_test.bootstrap_schema_failed(id bigint PRIMARY KEY)`)
	mustExecIntegration(t, dst, `CREATE TABLE cdc_test.bootstrap_effective(id bigint REFERENCES cdc_test.bootstrap_schema_failed(id))`)
	mustExecIntegration(t, dst, `CREATE TABLE cdc_test.bootstrap_unrelated(id bigint REFERENCES cdc_test.bootstrap_schema_failed(id))`)
	cfg := Config{JobID: fmt.Sprintf("bootstrap-%d", time.Now().UnixNano()), TargetDSN: integrationTargetDSN, TargetSchema: "cdc_test", LowerCaseNames: true}
	position := Position{File: "mysql-bin.000001", Pos: 120}
	if err := SaveTargetBootstrapRecord(ctx, cfg, BootstrapRecord{State: "snapshot_in_progress", Position: position}); err != nil {
		t.Fatal(err)
	}
	record := BootstrapRecord{
		State:                  "review_pending",
		Position:               position,
		EffectiveTables:        []string{"cdc_pk", "bootstrap_effective"},
		FailureReportVersion:   1,
		LocatorStrategyVersion: LocatorStrategyVersion,
		LocatorStrategies:      []LocatorStrategy{{Table: "cdc_pk", Strategy: LocatorPrimaryKey, Index: "PRIMARY", Columns: []string{"id"}}},
		FailedObjects: []BootstrapFailedObject{{
			Category: "table", Name: "bootstrap_failed", Error: "test", DDL: "CREATE TABLE bootstrap_failed(id badtype)", Stage: "schema",
		}},
		ExcludedTables: []BootstrapIssue{
			{Table: "bootstrap_failed", Stage: "data", Error: "test"},
			{Table: "bootstrap_schema_failed", Stage: "schema", Error: "test"},
		},
	}
	record.ManifestHash = HashBootstrapManifest(record)
	if err := SaveTargetBootstrapRecord(ctx, cfg, record); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := LoadTargetBootstrapRecord(ctx, cfg)
	if err != nil || !exists || loaded.State != "review_pending" || loaded.ManifestHash != record.ManifestHash || len(loaded.FailedObjects) != 1 {
		t.Fatalf("unexpected review checkpoint: exists=%v record=%+v err=%v", exists, loaded, err)
	}
	if err = FinalizeTargetBootstrap(ctx, cfg, record); err != nil {
		t.Fatal(err)
	}
	var failedTableExists bool
	if err = dst.QueryRowContext(ctx, `SELECT to_regclass('cdc_test.bootstrap_failed') IS NOT NULL`).Scan(&failedTableExists); err != nil || failedTableExists {
		t.Fatalf("excluded table was not removed: exists=%v err=%v", failedTableExists, err)
	}
	var effectiveFKs, unrelatedFKs int
	if err = dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid
		JOIN pg_namespace n ON n.oid=t.relnamespace WHERE c.contype='f' AND n.nspname='cdc_test' AND t.relname='bootstrap_effective'`).Scan(&effectiveFKs); err != nil || effectiveFKs != 0 {
		t.Fatalf("effective table foreign key to excluded table was not removed: count=%d err=%v", effectiveFKs, err)
	}
	if err = dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid
		JOIN pg_namespace n ON n.oid=t.relnamespace WHERE c.contype='f' AND n.nspname='cdc_test' AND t.relname='bootstrap_unrelated'`).Scan(&unrelatedFKs); err != nil || unrelatedFKs != 1 {
		t.Fatalf("unrelated table foreign key was unexpectedly removed: count=%d err=%v", unrelatedFKs, err)
	}
	loaded, exists, err = LoadTargetBootstrapRecord(ctx, cfg)
	if err != nil || !exists || loaded.State != "completed" || len(loaded.EffectiveTables) != 2 {
		t.Fatalf("unexpected completed checkpoint: exists=%v record=%+v err=%v", exists, loaded, err)
	}
	applier, err := NewPostgresApplier(ctx, cfg.TargetDSN, cfg.TargetSchema, cfg.JobID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer applier.Close()
	if err = applier.SaveCheckpoint(ctx, Position{File: position.File, Pos: 140}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err = applier.LoadBootstrapRecord(ctx)
	if err != nil || loaded.ManifestHash != record.ManifestHash || len(loaded.ExcludedTables) != 2 || len(loaded.FailedObjects) != 1 || loaded.FailureReportVersion != 1 || loaded.LocatorStrategyVersion != LocatorStrategyVersion || len(loaded.LocatorStrategies) != 1 {
		t.Fatalf("CDC checkpoint update overwrote bootstrap manifest or failure report: %+v err=%v", loaded, err)
	}
}

func TestCDCIntegration(t *testing.T) {
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("set CDC_INTEGRATION=1 and start testdata/docker-compose.yml")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	src := mustOpenIntegrationDB(t, "mysql", integrationSourceAdminDSN)
	defer src.Close()
	dst := mustOpenIntegrationDB(t, "postgres", integrationTargetDSN)
	defer dst.Close()

	setupIntegrationTables(t, ctx, src, dst)
	start, err := CurrentPosition(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		JobID:          fmt.Sprintf("integration-%d", time.Now().UnixNano()),
		SourceDSN:      integrationSourceCDCDSN,
		SourceHost:     "127.0.0.1",
		SourcePort:     13306,
		SourceUser:     "cdc",
		SourcePassword: "cdcpass",
		SourceDatabase: "cdc_source",
		TargetDSN:      integrationTargetDSN,
		TargetSchema:   "cdc_test",
		Mode:           "all",
		LowerCaseNames: true,
		ServerID:       uint32(2000000000 + time.Now().UnixNano()%1000000000),
		Start:          start,
	}
	tables, err := LoadTables(ctx, src, cfg.SourceDatabase, cfg.Mode, cfg.Filter)
	if err != nil {
		t.Fatal(err)
	}
	tables, err = ResolveLocatorStrategies(ctx, cfg, tables)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocatorStrategyVersion = LocatorStrategyVersion
	cfg.LocatorStrategies = LocatorStrategiesFromTables(tables)

	runCtx, err := Registry.Register(cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	stats := make(chan Stats, 64)
	runErr := make(chan error, 1)
	go func() {
		runErr <- NewRunner(cfg, Hooks{Stats: func(s Stats) {
			select {
			case stats <- s:
			default:
			}
		}}).Run(runCtx)
	}()
	waitCaughtUp(t, stats, 20*time.Second)

	tx, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExecIntegration(t, tx, `INSERT INTO cdc_pk(id,value_text) VALUES (1,'a'),(2,'b')`)
	mustExecIntegration(t, tx, `INSERT INTO cdc_no_pk(value_num) VALUES (10)`)
	mustExecIntegration(t, tx, `INSERT INTO cdc_unique(code,value_text) VALUES ('A','one')`)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExecIntegration(t, src, `UPDATE cdc_pk SET value_text='updated' WHERE id=1`)
	mustExecIntegration(t, src, `DELETE FROM cdc_pk WHERE id=2`)
	mustExecIntegration(t, src, `UPDATE cdc_no_pk SET value_num=11 WHERE value_num=10`)
	mustExecIntegration(t, src, `INSERT INTO cdc_no_pk(value_num) VALUES (30),(30)`)
	mustExecIntegration(t, src, `UPDATE cdc_no_pk SET value_num=31 WHERE value_num=30`)
	mustExecIntegration(t, src, `DELETE FROM cdc_no_pk WHERE value_num=31`)
	mustExecIntegration(t, src, `UPDATE cdc_unique SET code='B', value_text='two' WHERE code='A'`)
	mustExecIntegration(t, src, `DELETE FROM cdc_unique WHERE code='B'`)
	rollback, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExecIntegration(t, rollback, `INSERT INTO cdc_pk(id,value_text) VALUES (3,'rolled-back')`)
	if err = rollback.Rollback(); err != nil {
		t.Fatal(err)
	}

	head := waitTargetCheckpoint(t, ctx, cfg, src, 30*time.Second)
	var value string
	if err = dst.QueryRowContext(ctx, `SELECT value_text FROM cdc_test.cdc_pk WHERE id=1`).Scan(&value); err != nil || value != "updated" {
		t.Fatalf("updated row mismatch: value=%q err=%v", value, err)
	}
	var pkCount, noPKCount int
	if err = dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM cdc_test.cdc_pk`).Scan(&pkCount); err != nil || pkCount != 1 {
		t.Fatalf("primary-key table count=%d err=%v", pkCount, err)
	}
	if err = dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM cdc_test.cdc_no_pk`).Scan(&noPKCount); err != nil || noPKCount != 1 {
		t.Fatalf("no-PK INSERT was duplicated or lost: count=%d err=%v", noPKCount, err)
	}
	var noPKValue int
	if err = dst.QueryRowContext(ctx, `SELECT value_num FROM cdc_test.cdc_no_pk`).Scan(&noPKValue); err != nil || noPKValue != 11 {
		t.Fatalf("no-key UPDATE mismatch: value=%d err=%v", noPKValue, err)
	}
	var uniqueCount int
	if err = dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM cdc_test.cdc_unique`).Scan(&uniqueCount); err != nil || uniqueCount != 0 {
		t.Fatalf("unique-only UPDATE/DELETE mismatch: count=%d err=%v", uniqueCount, err)
	}

	if !Registry.RequestCutover(cfg.JobID, head) {
		t.Fatal("request cutover failed")
	}
	if err = waitRunnerError(t, runErr, 20*time.Second); !errors.Is(err, ErrCutoverReady) {
		t.Fatalf("runner error=%v, want ErrCutoverReady", err)
	}
	Registry.Remove(cfg.JobID)

	// Restart from the target checkpoint and verify a no-PK INSERT is applied
	// exactly once across the restart.
	runCtx, err = Registry.Register(cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	stats = make(chan Stats, 64)
	runErr = make(chan error, 1)
	go func() {
		runErr <- NewRunner(cfg, Hooks{Stats: func(s Stats) {
			select {
			case stats <- s:
			default:
			}
		}}).Run(runCtx)
	}()
	waitCaughtUp(t, stats, 20*time.Second)
	mustExecIntegration(t, src, `INSERT INTO cdc_no_pk(value_num) VALUES (20)`)
	waitTargetCheckpoint(t, ctx, cfg, src, 30*time.Second)
	if !Registry.Cancel(cfg.JobID, "pause") {
		t.Fatal("pause restarted runner failed")
	}
	if err = waitRunnerError(t, runErr, 20*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("pause error=%v", err)
	}
	Registry.Remove(cfg.JobID)
	if err = dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM cdc_test.cdc_no_pk`).Scan(&noPKCount); err != nil || noPKCount != 2 {
		t.Fatalf("checkpoint resume duplicated no-PK INSERT: count=%d err=%v", noPKCount, err)
	}

	// A source DDL must pause without moving the checkpoint past the DDL. After
	// acknowledgement, a fresh runner reloads column metadata.
	beforeDDL, exists, err := LoadTargetCheckpoint(ctx, cfg)
	if err != nil || !exists {
		t.Fatalf("load checkpoint before DDL: exists=%v err=%v", exists, err)
	}
	runCtx, err = Registry.Register(cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	ddlEvents := make(chan struct {
		sql string
		pos Position
	}, 1)
	runErr = make(chan error, 1)
	go func() {
		runErr <- NewRunner(cfg, Hooks{DDL: func(query string, pos Position) {
			ddlEvents <- struct {
				sql string
				pos Position
			}{query, pos}
		}}).Run(runCtx)
	}()
	mustExecIntegration(t, src, `ALTER TABLE cdc_pk ADD COLUMN extra_value BIGINT NULL`)
	ddl := waitDDLEvent(t, ddlEvents, 20*time.Second)
	if err = waitRunnerError(t, runErr, 20*time.Second); !errors.Is(err, ErrDDLPause) {
		t.Fatalf("DDL runner error=%v", err)
	}
	Registry.Remove(cfg.JobID)
	afterDDL, _, err := LoadTargetCheckpoint(ctx, cfg)
	if err != nil || !PositionEquivalent(beforeDDL, afterDDL) {
		t.Fatalf("checkpoint advanced past unacknowledged DDL: before=%+v after=%+v err=%v", beforeDDL, afterDDL, err)
	}
	mustExecIntegration(t, dst, `ALTER TABLE cdc_test.cdc_pk ADD COLUMN extra_value BIGINT NULL`)
	if err = AcknowledgeDDL(ctx, cfg, ddl.pos); err != nil {
		t.Fatal(err)
	}

	runCtx, err = Registry.Register(cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	runErr = make(chan error, 1)
	go func() { runErr <- NewRunner(cfg, Hooks{}).Run(runCtx) }()
	mustExecIntegration(t, src, `INSERT INTO cdc_pk(id,value_text,extra_value) VALUES (4,'after-ddl',44)`)
	waitTargetCheckpoint(t, ctx, cfg, src, 30*time.Second)
	if !Registry.Cancel(cfg.JobID, "pause") {
		t.Fatal("final pause failed")
	}
	_ = waitRunnerError(t, runErr, 20*time.Second)
	Registry.Remove(cfg.JobID)
	var extra int64
	if err = dst.QueryRowContext(ctx, `SELECT extra_value FROM cdc_test.cdc_pk WHERE id=4`).Scan(&extra); err != nil || extra != 44 {
		t.Fatalf("metadata was not refreshed after DDL: extra=%d err=%v", extra, err)
	}
}

func TestFullRowConflictPauseAndReplayIntegration(t *testing.T) {
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("set CDC_INTEGRATION=1 and start testdata/docker-compose.yml")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src := mustOpenIntegrationDB(t, "mysql", integrationSourceAdminDSN)
	defer src.Close()
	dst := mustOpenIntegrationDB(t, "postgres", integrationTargetDSN)
	defer dst.Close()
	setupIntegrationTables(t, ctx, src, dst)
	start, err := CurrentPosition(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JobID: fmt.Sprintf("conflict-%d", time.Now().UnixNano()), SourceDSN: integrationSourceCDCDSN,
		SourceHost: "127.0.0.1", SourcePort: 13306, SourceUser: "cdc", SourcePassword: "cdcpass",
		SourceDatabase: "cdc_source", TargetDSN: integrationTargetDSN, TargetSchema: "cdc_test", Mode: "all",
		LowerCaseNames: true, ServerID: uint32(1000000000 + time.Now().UnixNano()%1000000000), Start: start}
	tables, err := LoadTables(ctx, src, cfg.SourceDatabase, cfg.Mode, cfg.Filter)
	if err != nil {
		t.Fatal(err)
	}
	tables, err = ResolveLocatorStrategies(ctx, cfg, tables)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocatorStrategyVersion, cfg.LocatorStrategies = LocatorStrategyVersion, LocatorStrategiesFromTables(tables)

	runCtx, err := Registry.Register(cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	stats := make(chan Stats, 32)
	conflicts := make(chan RowConflict, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- NewRunner(cfg, Hooks{Stats: func(s Stats) {
			select {
			case stats <- s:
			default:
			}
		}, RowConflict: func(c RowConflict) { conflicts <- c }}).Run(runCtx)
	}()
	waitCaughtUp(t, stats, 20*time.Second)
	mustExecIntegration(t, src, `INSERT INTO cdc_no_pk(value_num) VALUES (100)`)
	waitTargetCheckpoint(t, ctx, cfg, src, 20*time.Second)
	checkpointBefore, exists, err := LoadTargetCheckpoint(ctx, cfg)
	if err != nil || !exists {
		t.Fatalf("load checkpoint before conflict: exists=%v err=%v", exists, err)
	}
	mustExecIntegration(t, dst, `DELETE FROM cdc_test.cdc_no_pk WHERE value_num=100`)
	mustExecIntegration(t, src, `UPDATE cdc_no_pk SET value_num=101 WHERE value_num=100`)
	select {
	case conflict := <-conflicts:
		if conflict.Table != "cdc_no_pk" || conflict.Action != "update" || len(conflict.BeforeHash) != 64 {
			t.Fatalf("unexpected conflict: %+v", conflict)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for row conflict")
	}
	if err = waitRunnerError(t, runErr, 20*time.Second); !errors.Is(err, ErrRowConflict) {
		t.Fatalf("runner error=%v, want ErrRowConflict", err)
	}
	Registry.Remove(cfg.JobID)
	checkpointAfter, _, err := LoadTargetCheckpoint(ctx, cfg)
	if err != nil || !PositionEquivalent(checkpointBefore, checkpointAfter) {
		t.Fatalf("checkpoint advanced on conflict: before=%+v after=%+v err=%v", checkpointBefore, checkpointAfter, err)
	}

	mustExecIntegration(t, dst, `INSERT INTO cdc_test.cdc_no_pk(value_num) VALUES (100)`)
	runCtx, err = Registry.Register(cfg.JobID)
	if err != nil {
		t.Fatal(err)
	}
	stats = make(chan Stats, 32)
	runErr = make(chan error, 1)
	go func() {
		runErr <- NewRunner(cfg, Hooks{Stats: func(s Stats) {
			select {
			case stats <- s:
			default:
			}
		}}).Run(runCtx)
	}()
	waitTargetCheckpoint(t, ctx, cfg, src, 20*time.Second)
	var value int
	if err = dst.QueryRowContext(ctx, `SELECT value_num FROM cdc_test.cdc_no_pk`).Scan(&value); err != nil || value != 101 {
		t.Fatalf("replayed row mismatch: value=%d err=%v", value, err)
	}
	if !Registry.Cancel(cfg.JobID, "pause") {
		t.Fatal("pause replay runner failed")
	}
	if err = waitRunnerError(t, runErr, 20*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("pause error=%v", err)
	}
	Registry.Remove(cfg.JobID)
}

type integrationExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func mustOpenIntegrationDB(t *testing.T, driver, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err = db.Ping(); err == nil {
			return db
		}
		if time.Now().After(deadline) {
			db.Close()
			t.Fatalf("wait for %s: %v", driver, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func setupIntegrationTables(t *testing.T, ctx context.Context, src, dst *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS cdc_pk`,
		`DROP TABLE IF EXISTS cdc_no_pk`,
		`DROP TABLE IF EXISTS cdc_unique`,
		`CREATE TABLE cdc_pk(id BIGINT PRIMARY KEY, value_text VARCHAR(100) NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE cdc_no_pk(value_num BIGINT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE cdc_unique(code VARCHAR(40) NOT NULL, value_text VARCHAR(100), UNIQUE KEY uq_code(code)) ENGINE=InnoDB`,
	} {
		mustExecIntegration(t, src, statement)
	}
	for _, statement := range []string{
		`DROP SCHEMA IF EXISTS cdc_test CASCADE`,
		`CREATE SCHEMA cdc_test`,
		`CREATE TABLE cdc_test.cdc_pk(id BIGINT PRIMARY KEY, value_text VARCHAR(100) NOT NULL)`,
		`CREATE TABLE cdc_test.cdc_no_pk(value_num BIGINT NOT NULL)`,
		`CREATE TABLE cdc_test.cdc_unique(code VARCHAR(40) NOT NULL, value_text VARCHAR(100), UNIQUE(code))`,
	} {
		mustExecIntegration(t, dst, statement)
	}
	start, err := CurrentPosition(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if result := Preflight(ctx, Config{SourceDSN: integrationSourceCDCDSN, SourceDatabase: "cdc_source", TargetDSN: integrationTargetDSN, TargetSchema: "cdc_test", Mode: "all", Start: start}, true); !result.OK {
		t.Fatalf("integration preflight failed: %+v", result.Errors)
	}
}

func mustExecIntegration(t *testing.T, db integrationExecer, statement string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), statement); err != nil {
		t.Fatalf("execute %q: %v", statement, err)
	}
}

func waitCaughtUp(t *testing.T, stats <-chan Stats, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case stat := <-stats:
			if stat.CaughtUp {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for runner to catch up")
		}
	}
}

func waitTargetCheckpoint(t *testing.T, ctx context.Context, cfg Config, src *sql.DB, timeout time.Duration) Position {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		head, err := CurrentPosition(ctx, src)
		if err == nil {
			checkpoint, exists, loadErr := LoadTargetCheckpoint(ctx, cfg)
			if loadErr == nil && exists && PositionReached(checkpoint, head) {
				return head
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("timed out waiting for target checkpoint")
	return Position{}
}

func waitRunnerError(t *testing.T, errs <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-time.After(timeout):
		t.Fatal("timed out waiting for runner to stop")
		return nil
	}
}

func waitDDLEvent(t *testing.T, events <-chan struct {
	sql string
	pos Position
}, timeout time.Duration) struct {
	sql string
	pos Position
} {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(timeout):
		t.Fatal("timed out waiting for DDL pause")
		return struct {
			sql string
			pos Position
		}{}
	}
}
