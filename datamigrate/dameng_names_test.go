package datamigrate

import (
	"context"
	"encoding/json"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Exercise the generic runner used by MySQL and PG-family sources as well as
// SQL Server's atomic preflight. The writer records every attempted CREATE batch.
type damengNamedReader struct {
	*mockReader
	kind string
}

func (r *damengNamedReader) DBType() string { return r.kind }

func TestDamengForbiddenNamesReportedBeforeAnyTableWrites(t *testing.T) {
	for _, src := range []string{"mysql", "postgres", "sqlserver"} {
		for _, content := range []string{"both", "schema_only"} {
			t.Run(src+"/"+content, func(t *testing.T) {
				r := &damengNamedReader{kind: src, mockReader: &mockReader{
					tables: []string{"gj_data"},
					ddl:    map[string]*source.TableDDLInfo{"gj_data": {TableName: "gj_data", Columns: []source.ColumnInfo{{Name: "RowId", DataType: "int", Extra: "auto_increment", IdentityInfo: &source.IdentityInfo{Seed: 1, Increment: 1}}}}},
					rows:   map[string][][]interface{}{"gj_data": {{1}}},
				}}
				w := &dmFixtureWriter{mockWriter: &mockWriter{}}
				m := dmMigrator(r, w)
				m.cfg.Content = content
				m.cfg.Mode = "all"
				report := m.Run(context.Background())
				require.True(t, report.HasFailures())
				require.Equal(t, 1, report.Tables.Failed)
				require.Empty(t, w.created, "must not attempt DROP or CREATE")
				require.Empty(t, w.copied)
				require.Zero(t, report.Data.Total)
				require.Len(t, report.Tables.Items, 1)
				item := report.Tables.Items[0]
				require.Contains(t, item.Error, "达梦禁止的列名: ROWID")
				require.Contains(t, item.DDL, `"ROWID" INT IDENTITY(1, 1) NOT NULL`)
				require.Contains(t, item.SuggestedDDL, `"SRC_ROWID" INT IDENTITY(1, 1) NOT NULL`)
				require.Contains(t, item.Suggestion, "不会自动应用此映射")
				require.Equal(t, "RowId", item.ColumnRenames[0].Source)
				payload, err := json.Marshal(report)
				require.NoError(t, err)
				var restored MigrationReport
				require.NoError(t, json.Unmarshal(payload, &restored))
				require.Equal(t, item, restored.Tables.Items[0])
			})
		}
	}
}

func TestSQLServerDamengForbiddenNameBlocksOtherTables(t *testing.T) {
	r, w := dmFixture()
	r.tables = append(r.tables, "Bad")
	r.ddl["Bad"] = &source.TableDDLInfo{TableName: "Bad", Columns: []source.ColumnInfo{{Name: "ROWID", DataType: "int"}}}
	report := dmMigrator(r, w).Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Empty(t, w.created, "valid tables must also wait for a successful SQL Server preflight")
	require.Empty(t, w.copied)
	require.NotEmpty(t, report.Tables.Items[0].SuggestedDDL)
}

func TestObjectResultOldReportCompatibility(t *testing.T) {
	var old ObjectResult
	require.NoError(t, json.Unmarshal([]byte(`{"name":"T","ddl":"CREATE TABLE T (id INT)","error":"old failure"}`), &old))
	require.Empty(t, old.SuggestedDDL)
	require.Empty(t, old.ColumnRenames)
	payload, err := json.Marshal(old)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "suggested_ddl")
	require.NotContains(t, string(payload), "suggestion")
	require.NotContains(t, string(payload), "column_renames")
}
