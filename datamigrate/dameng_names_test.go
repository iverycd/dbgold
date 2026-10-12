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

func TestDamengForbiddenNamesAutomaticallyRepaired(t *testing.T) {
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
				require.False(t, report.HasFailures())
				require.Equal(t, 1, report.Tables.Success)
				require.Len(t, w.created, 1)
				require.Contains(t, w.created[0], `"ROW_ID" INT IDENTITY(1, 1) NOT NULL`)
				require.Empty(t, report.Tables.Items)
				require.Len(t, report.AutoRepairs, 1)
				require.Equal(t, "RowId", report.AutoRepairs[0].Source)
				require.Equal(t, "ROW_ID", report.AutoRepairs[0].Target)
				require.Equal(t, "applied", report.AutoRepairs[0].Status)
				require.Equal(t, "RowId", r.ddl["gj_data"].Columns[0].Name)
				payload, err := json.Marshal(report)
				require.NoError(t, err)
				var restored MigrationReport
				require.NoError(t, json.Unmarshal(payload, &restored))
				require.Equal(t, report.AutoRepairs, restored.AutoRepairs)
			})
		}
	}
}

func TestSQLServerDamengForbiddenNameAllowsOtherTables(t *testing.T) {
	r, w := dmFixture()
	r.tables = append(r.tables, "Bad")
	r.ddl["Bad"] = &source.TableDDLInfo{TableName: "Bad", Columns: []source.ColumnInfo{{Name: "ROWID", DataType: "int"}}}
	report := dmMigrator(r, w).Run(context.Background())
	require.Zero(t, report.Tables.Failed)
	require.Len(t, w.created, 2)
	require.Len(t, report.AutoRepairs, 1)
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
