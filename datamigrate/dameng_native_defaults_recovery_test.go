package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

type nativeEmptyDefaultWriter struct {
	*nativeTestWriter
	emptyTables []damengnative.Table
	emptyErr    error
}

func (w *nativeEmptyDefaultWriter) CopyDamengNativeBatch(ctx context.Context, table damengnative.Table, rows [][]any) error {
	if len(rows) == 0 {
		w.emptyTables = append(w.emptyTables, table)
		if w.emptyErr != nil {
			return w.emptyErr
		}
	}
	return w.nativeTestWriter.CopyDamengNativeBatch(ctx, table, rows)
}

func TestNativeCopyRestoresEmptyTableDefaultsAfterSchemaMapping(t *testing.T) {
	for _, hasRowsInSnapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot_empty", true: "cursor_empty"}[hasRowsInSnapshot], func(t *testing.T) {
			_, r, base := nativeTestMigrator(Config{})
			w := &nativeEmptyDefaultWriter{nativeTestWriter: base}
			r.rows = nil
			table := r.snapshot.Tables[0]
			table.Rows = 0
			if hasRowsInSnapshot {
				table.Rows = 1
			}
			table.Columns[0].SQLType = `"Src"."Payload"`
			table.Columns[0].TypeSchema = "Src"
			table.Columns[0].DefaultOnNull = true
			table.Columns[0].DefaultSQL = `"Src"."Seq".NEXTVAL /* Src stays in this comment */`
			progress := 0
			n, err := copyDamengNativeTable(context.Background(), r, w, table, "Dst", 10, func(int64) error { progress++; return nil })
			require.NoError(t, err)
			require.Zero(t, n)
			require.Zero(t, progress, "default restoration is not a committed data batch")
			require.Len(t, w.emptyTables, 1)
			restored := w.emptyTables[0]
			require.Equal(t, "Dst", restored.Schema)
			require.Equal(t, "Dst", restored.Columns[0].TypeSchema)
			require.Equal(t, `"Dst"."Payload"`, restored.Columns[0].SQLType)
			require.Equal(t, `"Dst"."Seq".NEXTVAL /* Src stays in this comment */`, restored.Columns[0].DefaultSQL)
			require.Equal(t, `"Src"."Seq".NEXTVAL /* Src stays in this comment */`, table.Columns[0].DefaultSQL, "mapping must not alter the frozen source columns")
			require.Equal(t, map[bool]int{false: 0, true: 1}[hasRowsInSnapshot], r.scans)
			require.Empty(t, w.copied)
		})
	}
}

func TestNativeZeroRowRecoveryMustRestoreFrozenDefaultsBeforeSuccess(t *testing.T) {
	m, r, base := nativeTestMigrator(Config{CaptureRepair: true, TargetSchema: "Dst"})
	column := &r.snapshot.Tables[0].Columns[0]
	column.DefaultOnNull = true
	column.DefaultSQL = `"Src"."Seq".NEXTVAL`
	r.snapshot.Objects[0].Units[0].SQL = `CREATE TABLE "Src"."Mixed" ("Value" VARCHAR(100) DEFAULT ON NULL "Src"."Seq".NEXTVAL)`
	base.copyErr = errors.New("previous default restoration interrupted")
	w := &nativeEmptyDefaultWriter{nativeTestWriter: base}
	m.writer = w
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Len(t, report.RepairManifest.Objects, 1)
	obj := report.RepairManifest.Objects[0]
	require.True(t, obj.TrustedNative())
	require.True(t, obj.NativeTable.Columns[0].DefaultOnNull)
	require.Equal(t, `"Src"."Seq".NEXTVAL`, obj.NativeTable.Columns[0].DefaultSQL)

	// Whole-table recovery deliberately accepts current source data. The source
	// may now be empty while the failed prior batch left its default suspended.
	r.snapshot.Tables[0].Rows = 0
	r.rows = nil
	base.snapshot.Tables = append([]damengnative.Table{}, r.snapshot.Tables...)
	base.snapshot.Tables[0].Schema = "Dst"
	base.copyErr = nil
	w.emptyErr = errors.New("default still unavailable")
	preview, err := PreviewRecovery(context.Background(), r, w, m.cfg, obj, nil, nil)
	require.NoError(t, err)
	require.True(t, preview.DataRequired)
	require.Zero(t, preview.SourceRows)
	res, err := RunRecovery(context.Background(), r, w, m.cfg, preview, false, nil)
	require.ErrorContains(t, err, "default still unavailable")
	require.Equal(t, "failed", res.Status)
	require.NotEqual(t, "succeeded", res.DataStatus)
	require.Len(t, w.emptyTables, 1)
	require.Equal(t, `"Dst"."Seq".NEXTVAL`, w.emptyTables[0].Columns[0].DefaultSQL)

	w.emptyErr = nil
	res, err = RunRecovery(context.Background(), r, w, m.cfg, preview, false, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", res.Status)
	require.Equal(t, "succeeded", res.DataStatus)
	require.Zero(t, res.CommittedRows)
	require.Zero(t, res.TargetRows)
	require.Len(t, w.emptyTables, 2, "retry must recheck restoration even though no rows need insertion")
	require.Empty(t, w.copied)
	require.Equal(t, []int{0}, w.batchSizes)
	require.Equal(t, "failed", obj.DataStatus, "the frozen manifest should remain independent of recovery results")
}

// Keep the compiler checking that the lightweight recorder retains the native
// optional interface required by RunRecovery.
var _ damengnative.Writer = (*nativeEmptyDefaultWriter)(nil)
