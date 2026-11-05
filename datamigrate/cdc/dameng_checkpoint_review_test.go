package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDamengReviewSaveKeepsFrozenCleanupIdentityIntegration(t *testing.T) {
	dsn := os.Getenv("CDC_DAMENG_DSN")
	if dsn == "" {
		t.Skip("set CDC_DAMENG_DSN to a disposable DM instance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	schema := fmt.Sprintf("CDC_REVIEW_%d", time.Now().UnixNano())
	exec := func(statement string) { t.Helper(); _, err := db.ExecContext(ctx, statement); require.NoError(t, err) }
	exec("CREATE SCHEMA " + schema + " AUTHORIZATION SYSDBA")
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
	exec("CREATE TABLE " + schema + ".FAILED(ID INT)")
	a, err := newDamengApplier(ctx, Config{TargetDBType: "dameng", TargetDSN: dsn, TargetSchema: schema, JobID: "review"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	review := BootstrapRecord{State: "review_pending", Position: Position{File: "mysql-bin.000001", Pos: 120}, EffectiveTables: []string{"Good"}, ExcludedTables: []BootstrapIssue{{Table: "Failed", Stage: "data", Error: "copy failed"}}}
	review.ManifestHash = HashBootstrapManifest(review)
	require.NoError(t, a.SaveBootstrapRecord(ctx, review))
	exec("DROP TABLE " + schema + ".FAILED")
	exec("CREATE TABLE " + schema + ".FAILED(ID INT,KEEP_ME INT)")
	// Retrying report persistence must not authorize cleanup of the new object.
	require.NoError(t, a.SaveBootstrapRecord(ctx, review))
	require.ErrorContains(t, a.FinalizeBootstrap(ctx, review), "已被替换")
	var keepColumn int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME='FAILED' AND COLUMN_NAME='KEEP_ME'`, schema).Scan(&keepColumn))
	require.Equal(t, 1, keepColumn)

	t.Run("completed report replay cannot rewind CDC", func(t *testing.T) {
		a.jobID = "completed"
		completed := BootstrapRecord{State: "completed", Position: review.Position, EffectiveTables: []string{"Good"}}
		completed.ManifestHash = HashBootstrapManifest(completed)
		require.NoError(t, a.SaveBootstrapRecord(ctx, completed))
		advanced := Position{File: completed.Position.File, Pos: 500}
		require.NoError(t, a.SaveCheckpoint(ctx, advanced))
		require.NoError(t, a.SaveBootstrapRecord(ctx, completed))
		position, _, err := a.LoadCheckpoint(ctx)
		require.NoError(t, err)
		require.Equal(t, advanced, position)
		completed.State = "review_pending"
		require.ErrorContains(t, a.SaveBootstrapRecord(ctx, completed), "不能覆盖")
	})
	t.Run("aborted report cannot resume cleanup", func(t *testing.T) {
		a.jobID = "aborted"
		aborted := BootstrapRecord{State: "aborted", Position: review.Position, EffectiveTables: []string{"Good"}}
		aborted.ManifestHash = HashBootstrapManifest(aborted)
		require.NoError(t, a.SaveBootstrapRecord(ctx, aborted))
		require.NoError(t, a.SaveBootstrapRecord(ctx, aborted))
		aborted.State = "snapshot_in_progress"
		require.ErrorContains(t, a.SaveBootstrapRecord(ctx, aborted), "不能覆盖")
	})
}
