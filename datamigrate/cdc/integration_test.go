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
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExecIntegration(t, src, `UPDATE cdc_pk SET value_text='updated' WHERE id=1`)
	mustExecIntegration(t, src, `DELETE FROM cdc_pk WHERE id=2`)
	mustExecIntegration(t, src, `UPDATE cdc_no_pk SET value_num=11 WHERE value_num=10`)
	rollback, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExecIntegration(t, rollback, `INSERT INTO cdc_pk(id,value_text) VALUES (3,'rolled-back')`)
	if err = rollback.Rollback(); err != nil {
		t.Fatal(err)
	}

	head := waitTargetCheckpoint(t, ctx, cfg, src, 30*time.Second)
	waitSkippedEvent(t, stats, 20*time.Second)
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
		`CREATE TABLE cdc_pk(id BIGINT PRIMARY KEY, value_text VARCHAR(100) NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE cdc_no_pk(value_num BIGINT NOT NULL) ENGINE=InnoDB`,
	} {
		mustExecIntegration(t, src, statement)
	}
	for _, statement := range []string{
		`DROP SCHEMA IF EXISTS cdc_test CASCADE`,
		`CREATE SCHEMA cdc_test`,
		`CREATE TABLE cdc_test.cdc_pk(id BIGINT PRIMARY KEY, value_text VARCHAR(100) NOT NULL)`,
		`CREATE TABLE cdc_test.cdc_no_pk(value_num BIGINT NOT NULL)`,
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

func waitSkippedEvent(t *testing.T, stats <-chan Stats, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case stat := <-stats:
			if stat.Skipped > 0 && stat.Warnings > 0 {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for visible no-primary-key warning")
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
