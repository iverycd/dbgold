package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDamengCheckpointRefusesPendingBootstrap(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	a := &DamengApplier{db: db, cfg: Config{TargetDBType: "dameng"}, schema: "APP", jobID: "job"}
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "APP"."__DBGOLD_CDC_CHECKPOINT" SET BINLOG_FILE=?,BINLOG_POSITION=?,GTID=?,UPDATED_AT=CURRENT_TIMESTAMP WHERE JOB_ID=? AND BOOTSTRAP_STATE='completed'`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COUNT(*) FROM "APP"."__DBGOLD_CDC_CHECKPOINT" WHERE JOB_ID=?`).WithArgs("job").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()
	require.ErrorContains(t, a.SaveCheckpoint(t.Context(), Position{File: "mysql-bin.000001", Pos: 12}), "尚未完成")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDamengCheckpointIntegration(t *testing.T) {
	dsn := os.Getenv("CDC_DAMENG_DSN")
	if dsn == "" {
		t.Skip("set CDC_DAMENG_DSN to a disposable DM instance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema := fmt.Sprintf("CDC_CP_%d", time.Now().UnixNano())
	exec := func(s string) { t.Helper(); _, e := db.ExecContext(ctx, s); require.NoError(t, e) }
	exec("CREATE SCHEMA " + schema + " AUTHORIZATION SYSDBA")
	defer func() { _, e := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, e) }()
	cfg := Config{TargetDBType: "dameng", TargetDSN: dsn, TargetSchema: schema, JobID: "checkpoint"}
	a, err := newDamengApplier(ctx, cfg)
	require.NoError(t, err)
	defer a.Close()
	r := BootstrapRecord{State: "completed", Position: Position{File: "mysql-bin.000001", Pos: 120}, EffectiveTables: []string{"Mixed"}, FailureReportVersion: 1, LocatorStrategyVersion: LocatorStrategyVersion, LocatorStrategies: []LocatorStrategy{{Table: "Mixed", Strategy: LocatorFullRow, Columns: []string{"Id"}}}, FailedObjects: []BootstrapFailedObject{{Category: "table", Name: "Bad", DDL: strings.Repeat("中文 DDL\n", 10000), Error: "failure"}}}
	r.ManifestHash = HashBootstrapManifest(r)
	require.NoError(t, a.SaveBootstrapRecord(ctx, r))
	loaded, exists, err := a.LoadBootstrapRecord(ctx)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, r, loaded)
	require.NoError(t, a.SaveCheckpoint(ctx, Position{File: r.Position.File, Pos: 140, GTID: strings.Repeat("abc", 5000)}))
	loaded, _, err = a.LoadBootstrapRecord(ctx)
	require.NoError(t, err)
	require.Equal(t, r.ManifestHash, loaded.ManifestHash)
	require.Equal(t, r.FailedObjects, loaded.FailedObjects)
	require.Len(t, loaded.Position.GTID, 15000)
	require.NoError(t, a.SaveCheckpoint(ctx, Position{File: r.Position.File, Pos: 160}))
	pos, _, err := a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.Empty(t, pos.GTID)

	t.Run("recover cleanup after drop before progress save", func(t *testing.T) {
		exec("CREATE TABLE " + schema + ".BAD(ID INT PRIMARY KEY)")
		exec("CREATE TABLE " + schema + ".GOOD(ID INT REFERENCES " + schema + ".BAD(ID))")
		exec("CREATE TABLE " + schema + ".SCHEMA_FAILED(ID INT)")
		a.jobID = "cleanup"
		review := BootstrapRecord{State: "review_pending", Position: r.Position, EffectiveTables: []string{"Good"}, ExcludedTables: []BootstrapIssue{{Table: "Bad", Stage: "data", Error: "data failed"}, {Table: "Schema_Failed", Stage: "schema", Error: "create failed"}}, LocatorStrategyVersion: LocatorStrategyVersion}
		review.ManifestHash = HashBootstrapManifest(review)
		require.NoError(t, a.SaveBootstrapRecord(ctx, review))
		require.ErrorContains(t, a.SaveCheckpoint(ctx, Position{Pos: 1000}), "尚未完成")
		// Simulate loss of the process after a cleanup DDL implicitly committed,
		// before the progress UPDATE was persisted.
		plan, err := a.planBootstrapCleanup(ctx, review)
		require.NoError(t, err)
		require.Len(t, plan.Items, 2)
		exec("ALTER TABLE " + schema + ".GOOD DROP CONSTRAINT " + quoteIdent(plan.Items[0].Constraint))
		require.NoError(t, a.FinalizeBootstrap(ctx, review))
		require.NoError(t, a.FinalizeBootstrap(ctx, review))
		loaded, _, err := a.LoadBootstrapRecord(ctx)
		require.NoError(t, err)
		require.Equal(t, "completed", loaded.State)
		require.Equal(t, review.ExcludedTables, loaded.ExcludedTables)
		_, exists, err := a.cleanupObjectID(ctx, damengCleanupItem{Table: "BAD"})
		require.NoError(t, err)
		require.False(t, exists)
		_, exists, err = a.cleanupObjectID(ctx, damengCleanupItem{Table: "SCHEMA_FAILED"})
		require.NoError(t, err)
		require.True(t, exists)
	})
	t.Run("review never deletes a replacement object", func(t *testing.T) {
		exec("CREATE TABLE " + schema + ".REPLACED(ID INT)")
		a.jobID = "replacement"
		review := BootstrapRecord{State: "review_pending", Position: r.Position, EffectiveTables: []string{"Good"}, ExcludedTables: []BootstrapIssue{{Table: "Replaced", Stage: "data", Error: "data failed"}}}
		review.ManifestHash = HashBootstrapManifest(review)
		require.NoError(t, a.SaveBootstrapRecord(ctx, review))
		exec("DROP TABLE " + schema + ".REPLACED")
		exec("CREATE TABLE " + schema + ".REPLACED(ID INT, KEEP_ME INT)")
		require.ErrorContains(t, a.FinalizeBootstrap(ctx, review), "已被替换")
		loaded, _, err := a.LoadBootstrapRecord(ctx)
		require.NoError(t, err)
		require.Equal(t, "review_pending", loaded.State)
		_, exists, err := a.cleanupObjectID(ctx, damengCleanupItem{Table: "REPLACED"})
		require.NoError(t, err)
		require.True(t, exists)
	})
}
