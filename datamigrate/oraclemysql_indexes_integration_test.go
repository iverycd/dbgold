package datamigrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// The existing integration opt-in and unique-name cleanup helpers restrict
// these fixtures to the dedicated Oracle -> MySQL test database.
type oracleMySQLIndexLiveReader struct {
	*oracleMySQLLiveReader
	indexes []source.IndexInfo
}

func (r *oracleMySQLIndexLiveReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return cloneOracleMySQLIndexes(r.indexes), nil
}

func TestOracleMySQLLiveLongOrdinaryIndexUsesPrefixWithoutTruncatingData(t *testing.T) {
	ctx, db, w, schema, prefix := oracleMySQLLiveTarget(t)
	profile, err := w.InspectMySQLIndexProfile(ctx)
	require.NoError(t, err)
	if profile.KeyLimit("DYNAMIC") != 3072 {
		t.Skip("the 768-character regression requires a 3072-byte InnoDB key limit")
	}
	table, index := prefix+"_long_url", prefix+"_ix_url"
	oracleMySQLLiveCleanup(t, db, schema, []string{table})
	payload := strings.Repeat("中😀", 450) // 900 characters; the index uses only 768.
	r := &oracleMySQLIndexLiveReader{
		oracleMySQLLiveReader: &oracleMySQLLiveReader{
			tables: []source.TableDDLInfo{{TableName: table, Columns: []source.ColumnInfo{
				{Name: "id", DataType: "NUMBER", Precision: 9},
				{Name: "moduleurl", DataType: "VARCHAR2", Length: 2000, IsNullable: true},
			}}},
			keys: []source.IndexInfo{{TableName: table, IndexName: "PRIMARY", Columns: []string{"id"}, IsPrimary: true, IsUnique: true}},
			data: map[string][][]any{table: {{int64(1), payload}, {int64(2), payload + "tail"}}},
		},
		indexes: []source.IndexInfo{{TableName: table, IndexName: index, Columns: []string{"moduleurl"}}},
	}
	job := &Job{LogCh: make(chan string, 4096)}
	m := NewMigrator(r, w, job, Config{
		TargetDBType: "mysql", TargetSchema: schema, Mode: "include", Filter: table,
		Content: "both", CaptureRepair: true, PageSize: 1, MaxParallel: 1,
	})
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "report=%+v logs=%v", report, drainLogs(job))
	require.Equal(t, 1, report.Indexes.Success)
	var subPart sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT SUB_PART FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND INDEX_NAME=? AND SEQ_IN_INDEX=1`, schema, table, index).Scan(&subPart))
	require.True(t, subPart.Valid)
	require.Equal(t, int64(768), subPart.Int64)
	var columnType string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='moduleurl'`, schema, table).Scan(&columnType))
	require.Equal(t, "varchar(2000)", columnType)
	for i, want := range []string{payload, payload + "tail"} {
		var got string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT `moduleurl` FROM "+oracleMySQLLiveQualified(schema, table)+" WHERE `id`=?", i+1).Scan(&got))
		require.Equal(t, want, got)
	}
	var warned bool
	for _, warning := range report.Warnings {
		warned = warned || warning.Table == table && strings.Contains(warning.Reason, "前缀索引")
	}
	require.True(t, warned)
	require.NotNil(t, report.RepairManifest)
	var captured bool
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "index" && object.TargetName == index {
			captured = true
			require.Contains(t, object.DDL, "`moduleurl`(768)")
			require.Equal(t, "succeeded", object.Status)
		}
	}
	require.True(t, captured)
}
