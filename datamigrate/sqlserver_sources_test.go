package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type sqlServerCrossReader struct {
	*sqlServerFixtureReader
	profile map[string][]string
}

func (r sqlServerCrossReader) MigrationSchema() string { return "Src" }

func (r sqlServerCrossReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}

func (r sqlServerCrossReader) SQLServerDecimalProfile(_ context.Context, _ string, columns []string, collect func(string, interface{}) error) error {
	for _, name := range columns {
		for _, value := range r.profile[name] {
			if err := collect(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

type sqlServerCrossWriter struct {
	*sqlServerRecordingWriter
	precision, scale int64
}

func (w sqlServerCrossWriter) CountRows(_ context.Context, t string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var n int64
	for _, table := range w.copied {
		if t == table {
			n++
		}
	}
	return n, nil
}

func (w sqlServerCrossWriter) SQLServerExistingDecimal(context.Context, string, string) (int64, int64, error) {
	return w.precision, w.scale, nil
}

func sqlServerCrossFixture(srcType string) (sqlServerCrossReader, sqlServerCrossWriter) {
	r, w, _ := sqlServerFixture()
	r.srcType = srcType
	r.sequences = nil
	for _, info := range r.ddl {
		info.Columns[0].Extra = ""
		if srcType == "oracle" {
			info.Columns[0].DataType, info.Columns[0].Precision = "NUMBER", 9
		} else {
			info.Columns[0].DataType = "int4"
		}
	}
	r.views = nil
	return sqlServerCrossReader{sqlServerFixtureReader: r}, sqlServerCrossWriter{sqlServerRecordingWriter: w}
}

func TestOraclePGSQLServerCreateFailureContinuesIndependentTables(t *testing.T) {
	for _, srcType := range []string{"oracle", "postgres"} {
		for _, failed := range []string{"First", "Items"} {
			t.Run(srcType+"/"+failed, func(t *testing.T) {
				r, w := sqlServerCrossFixture(srcType)
				r.tables = append(r.tables, "First")
				r.ddl["First"] = &source.TableDDLInfo{TableName: "First", Columns: r.ddl["Orders"].Columns}
				r.rows["First"] = r.rows["Orders"]
				r.foreign = []source.FKInfo{{TableName: "Orders", RefSchema: "Src", RefTable: failed, ConstraintName: "fk_failed", Columns: []string{"id"}, RefColumns: []string{"id"}}}
				w.createErrors = map[string]error{failed: errors.New("CREATE failed")}
				m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", Mode: "all", MaxParallel: 1})
				report := m.Run(context.Background())
				require.Equal(t, 2, report.Tables.Success)
				require.Equal(t, 2, report.Data.Success)
				require.Zero(t, r.reads[failed])
				require.Contains(t, w.copied, "Orders")
				require.Contains(t, report.Tables.Items[0].DDL, "CREATE TABLE [dbo].["+failed+"]")
				require.Contains(t, report.Constraints.Items[0].Error, "依赖表")
			})
		}
	}
}

func TestOraclePGSQLServerCountMismatchRetainsComparedCounts(t *testing.T) {
	r, w := sqlServerCrossFixture("postgres")
	// This fixture records one destination row per batch. Two source rows
	// written in the same batch therefore exercise the count mismatch path.
	r.rows["Orders"] = append(r.rows["Orders"], r.rows["Orders"][0])
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", Mode: "all", MaxParallel: 1})
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Data.Failed)
	require.True(t, report.HasFailures())
	require.Contains(t, report.RowCounts, TableRowCount{Table: "Orders", Src: 2, Dst: 1})
	require.Len(t, report.DataIssues, 1)
	require.Equal(t, rowCountMismatch, report.DataIssues[0].Kind)
}

func TestSQLServerOraclePGExactDecimalPlanning(t *testing.T) {
	for _, test := range []struct{ src, number, def string }{
		{"oracle", "NUMBER", "-12.00012500"},
		{"postgres", "numeric", "(-12.00012500)::numeric"},
	} {
		t.Run(test.src, func(t *testing.T) {
			r, w := sqlServerCrossFixture(test.src)
			r.profile = map[string][]string{"amount": {"-3.021", "1.0E+6"}}
			cols := []source.ColumnInfo{{Name: "amount", DataType: test.number, Default: &test.def}, {Name: "empty", DataType: test.number, IsNullable: true}}
			name := func(s string) string { return s }
			warnings, err := planSQLServerDecimals(context.Background(), r, w, "Orders", "Orders", name, cols, false, nil)
			require.NoError(t, err)
			require.Len(t, warnings, 2)
			require.EqualValues(t, 38, cols[0].SQLServerPrecision)
			require.EqualValues(t, 6, cols[0].SQLServerScale)
			require.EqualValues(t, 18, cols[1].SQLServerScale)

			w.precision, w.scale = 10, 2
			_, err = planSQLServerDecimals(context.Background(), r, w, "Orders", "Orders", name, cols, true, nil)
			require.ErrorContains(t, err, "DECIMAL(10,2)")

			r.profile["amount"] = []string{strings.Repeat("9", 38), "0.1"}
			cols[0].SQLServerPrecision, cols[0].SQLServerScale = 0, 0
			_, err = planSQLServerDecimals(context.Background(), r, w, "Orders", "Orders", name, cols, false, nil)
			require.ErrorContains(t, err, "无法无损写入")
		})
	}
}

func TestPostgresSQLServer2008PlansColumnIdentityAndReportsStandaloneSequence(t *testing.T) {
	r, w := sqlServerCrossFixture("postgres")
	r.sequences = []source.SequenceInfo{
		{StartValueText: "7", Native: &source.NativeSequenceInfo{
			Schema: "Src", Name: "Owned", MinValue: "1", MaxValue: "2147483647", IncrementBy: "1",
			Bindings: []source.SequenceBinding{{Schema: "Src", TableName: "Orders", ColumnName: "id", Kind: "serial"}},
		}},
		{StartValueText: "10", Native: &source.NativeSequenceInfo{Schema: "Src", Name: "Independent", MinValue: "1", MaxValue: "99", IncrementBy: "1"}},
	}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", Mode: "all", Content: "schema_only"})
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Sequences.Success)
	require.Equal(t, 1, report.Sequences.Failed)
	require.Contains(t, report.Sequences.Items[0].Error, "SQL Server 2008 R2")
	require.Contains(t, strings.Join(w.sql, "\n"), "[id] INT IDENTITY(7,1)")
	require.Contains(t, report.Warnings[0].Reason, "IDENTITY_INSERT")
}

func TestSQLServerRawViewDefinitionIsDiagnosticOnly(t *testing.T) {
	r, w := sqlServerCrossFixture("postgres")
	r.views = []source.ViewInfo{
		{ViewName: "Rejected", Definition: `SELECT "id" FROM outside."Orders"`},
		{ViewName: "Accepted", Definition: `SELECT "id" FROM "Src"."Orders"`, Columns: r.ddl["Orders"].Columns},
	}
	var manifest RepairManifest
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", Mode: "all", Content: "schema_only", ViewNames: []string{"Rejected", "Accepted"}, CaptureRepair: true, RepairSink: func(v RepairManifest) error { manifest = v; return nil }})
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Views.Success, "%+v", report)
	require.Equal(t, 1, report.Views.Failed)
	require.Equal(t, r.views[0].Definition, report.Views.Items[0].SourceDDL)
	require.Empty(t, report.Views.Items[0].DDL)
	require.Len(t, manifest.Objects, 2)
	require.NotContains(t, strings.Join(w.sql, "\n"), "CREATE TABLE")
	for _, item := range manifest.Objects {
		if item.Name == "Rejected" {
			require.Empty(t, item.Units)
			require.Equal(t, r.views[0].Definition, item.SourceDDL)
		} else {
			require.Contains(t, item.DDL, "CREATE VIEW [dbo].[Accepted]")
			require.Contains(t, item.DDL, "[dbo].[Orders]")
		}
	}
}

func TestOnlyUntranslatedCrossViewIsSavedForManualRepair(t *testing.T) {
	r, w := sqlServerCrossFixture("postgres")
	raw := `SELECT "id" FROM outside."Orders"`
	r.views = []source.ViewInfo{{ViewName: "Rejected", Definition: raw}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{TargetDBType: "sqlserver", Mode: "all", Content: "schema_only", ViewNames: []string{"Rejected"}, CaptureRepair: true})
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Views.Failed)
	require.NotNil(t, report.RepairManifest)
	require.Len(t, report.RepairManifest.Objects, 1)
	obj := report.RepairManifest.Objects[0]
	require.Equal(t, raw, obj.SourceDDL)
	require.Empty(t, obj.Units)
	require.Equal(t, "failed", obj.Status)
	require.True(t, obj.CanEditDDL())
	require.Empty(t, w.sql)
}
