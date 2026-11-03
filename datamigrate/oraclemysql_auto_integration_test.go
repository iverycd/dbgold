package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type oracleMySQLAutoLiveReader struct {
	*oracleMySQLLiveReader
	bindings []source.OracleMySQLAutoIncrement
}

func (r *oracleMySQLAutoLiveReader) GetOracleMySQLAutoIncrements(context.Context) ([]source.OracleMySQLAutoIncrement, error) {
	return r.bindings, nil
}
func (r *oracleMySQLAutoLiveReader) GetSequences(context.Context) ([]source.SequenceInfo, error) {
	var out []source.SequenceInfo
	for _, a := range r.bindings {
		copy := a
		out = append(out, source.SequenceInfo{TableName: a.TableName, ColumnName: a.ColumnName, StartValueText: a.StartValueText, OracleMySQL: &copy})
	}
	return out, nil
}
func (r *oracleMySQLAutoLiveReader) GetTriggerCount(context.Context) (int, error) {
	return len(r.bindings), nil
}

func TestOracleMySQLAutoLivePreservesIDsAndCalibratesSharedSequence(t *testing.T) {
	ctx, db, w, schema, prefix := oracleMySQLLiveTarget(t)
	names := []string{prefix + "_auto_a", prefix + "_auto_b", prefix + "_auto_empty"}
	oracleMySQLLiveCleanup(t, db, schema, names)
	r := &oracleMySQLAutoLiveReader{oracleMySQLLiveReader: &oracleMySQLLiveReader{data: map[string][][]any{}}}
	for _, name := range names {
		r.tables = append(r.tables, source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "ID", DataType: "NUMBER", Extra: "auto_increment", OracleMySQLAutoType: "bigint"}}})
		r.keys = append(r.keys, source.IndexInfo{TableName: name, IndexName: "PK_" + name, Columns: []string{"ID"}, IsPrimary: true, IsUnique: true})
		r.bindings = append(r.bindings, source.OracleMySQLAutoIncrement{TableName: name, ColumnName: "ID", TriggerName: "TG_" + name, SequenceOwner: "APP", SequenceName: "HIBERNATE_SEQUENCE", StartValueText: "9007199254742000", SharedSequence: true})
		if !strings.HasSuffix(name, "empty") {
			r.data[name] = [][]any{{"0"}, {"9007199254740993"}, {"-1"}}
		}
	}
	job := &Job{LogCh: make(chan string, 4096)}
	m := NewMigrator(r, w, job, Config{TargetDBType: "mysql", TargetSchema: schema, Content: "both", Mode: "all", CaptureRepair: true, PageSize: 1, MaxParallel: 2})
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "report=%+v logs=%v", report, drainLogs(job))
	require.Equal(t, 3, report.TriggerAdaptation.Success)
	for _, name := range names {
		q := oracleMySQLLiveQualified(schema, name)
		if !strings.HasSuffix(name, "empty") {
			var ids string
			require.NoError(t, db.QueryRowContext(ctx, "SELECT GROUP_CONCAT(CAST(`ID` AS CHAR) ORDER BY `ID`) FROM "+q).Scan(&ids))
			require.Equal(t, "-1,0,9007199254740993", ids)
		}
		var extra string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='ID'`, schema, name).Scan(&extra))
		require.Contains(t, extra, "auto_increment")
		result, err := db.ExecContext(ctx, "INSERT INTO "+q+" () VALUES ()")
		require.NoError(t, err)
		id, err := result.LastInsertId()
		require.NoError(t, err)
		require.Equal(t, int64(9007199254742000), id)
		// Recovery recalculates from current state even when the saved seed is old.
		seq := source.SequenceInfo{TableName: name, ColumnName: "ID", StartValueText: "800", OracleMySQL: &source.OracleMySQLAutoIncrement{}}
		require.NoError(t, w.CreateSequence(ctx, seq, ""))
		result, err = db.ExecContext(ctx, "INSERT INTO "+q+" () VALUES ()")
		require.NoError(t, err)
		id, err = result.LastInsertId()
		require.NoError(t, err)
		require.Equal(t, int64(9007199254742001), id, fmt.Sprint(report.TriggerAdaptation))
	}
	// A data-only reload must retain the advanced target counter. Only this
	// fixture's rows are removed; the production workflow never clears them.
	for i, name := range names {
		_, err := db.ExecContext(ctx, "DELETE FROM "+oracleMySQLLiveQualified(schema, name))
		require.NoError(t, err)
		r.bindings[i].StartValueText = "1"
	}
	dataJob := &Job{LogCh: make(chan string, 4096)}
	dataMigrator := NewMigrator(r, w, dataJob, Config{TargetDBType: "mysql", TargetSchema: schema, Content: "data_only", Mode: "all", CaptureRepair: true, PageSize: 1, MaxParallel: 2})
	dataReport := dataMigrator.Run(ctx)
	require.False(t, dataReport.HasFailures(), "%+v logs=%v", dataReport, drainLogs(dataJob))
	require.Equal(t, 3, dataReport.TriggerAdaptation.Success)
	for _, item := range dataReport.TriggerAdaptation.Items {
		require.Equal(t, "reused", item.ConversionStatus)
		require.Equal(t, "9007199254742002", item.Start)
	}
	objectJob := &Job{LogCh: make(chan string, 4096)}
	objectMigrator := NewMigrator(r, w, objectJob, Config{TargetDBType: "mysql", TargetSchema: schema, Content: "schema_only", Mode: "all", Objects: []string{"sequences"}, CaptureRepair: true})
	objectReport := objectMigrator.Run(ctx)
	require.False(t, objectReport.HasFailures(), "%+v logs=%v", objectReport, drainLogs(objectJob))
	require.Equal(t, 3, objectReport.TriggerAdaptation.Success)
	for _, item := range objectReport.TriggerAdaptation.Items {
		require.Equal(t, "9007199254742002", item.Start)
	}
}
