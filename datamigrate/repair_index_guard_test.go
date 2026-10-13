package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type repairIndexReader struct {
	*mockReader
	indexes []source.IndexInfo
}

func (r *repairIndexReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return r.indexes, nil
}

type repairIndexDialect struct {
	dialect.Dialect
	generated []source.IndexInfo
}

func (d *repairIndexDialect) IndexStatements(schema string, idx source.IndexInfo) []dialect.Statement {
	d.generated = append(d.generated, idx)
	return d.Dialect.IndexStatements(schema, idx)
}

type repairIndexWriter struct {
	*mockWriter
	dia     *repairIndexDialect
	created []source.IndexInfo
}

func (w *repairIndexWriter) Dialect() dialect.Dialect { return w.dia }
func (w *repairIndexWriter) CreateIndex(_ context.Context, idx source.IndexInfo) error {
	w.created = append(w.created, idx)
	return nil
}

func newRepairIndexWriter() *repairIndexWriter {
	return &repairIndexWriter{mockWriter: &mockWriter{}, dia: &repairIndexDialect{Dialect: dialect.NewPostgres("postgres")}}
}

func TestRepairUnsupportedIndexesNeverGenerateOrExecuteSQL(t *testing.T) {
	const reason = "表达式索引暂不支持自动转换"
	r := &repairIndexReader{
		mockReader: &mockReader{tables: []string{"MixedTable"}, ddl: map[string]*source.TableDDLInfo{
			"MixedTable": {TableName: "MixedTable", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}},
		}},
		indexes: []source.IndexInfo{
			{TableName: "MixedTable", IndexName: "IxNormal", Columns: []string{"ID"}},
			{TableName: "MixedTable", IndexName: "IxExpression", Unsupported: reason},
		},
	}
	w := newRepairIndexWriter()
	m, _ := newTestMigrator(r, w)
	m.cfg.LowerCaseNames, m.cfg.TargetSchema = true, "dst"
	m.beginRepair(context.Background())
	require.Len(t, w.dia.generated, 1)
	require.Equal(t, []string{"id"}, w.dia.generated[0].Columns)
	var normal, unsupported repair.Object
	for _, o := range m.repairs.objects {
		if o.Kind != "index" {
			continue
		}
		if o.Index.Unsupported != "" {
			unsupported = o
		} else {
			normal = o
		}
	}
	require.NotEmpty(t, normal.DDL)
	require.Len(t, normal.Units, 1)
	require.Equal(t, "IxExpression", unsupported.SourceName)
	require.Equal(t, "MixedTable", unsupported.SourceTable)
	require.Empty(t, unsupported.DDL)
	require.Empty(t, unsupported.Units)
	require.Equal(t, reason, unsupported.Error)

	require.ErrorContains(t, m.repairIndex(context.Background(), *unsupported.Index), reason)
	require.NoError(t, m.repairIndex(context.Background(), *normal.Index))
	require.Len(t, w.dia.generated, 2)
	require.Len(t, w.created, 1)
	require.Equal(t, "ixnormal", w.created[0].IndexName)
	for _, o := range m.repairs.objects {
		if o.ID == unsupported.ID {
			require.Equal(t, "failed", o.Status)
			require.Empty(t, o.DDL)
			require.Empty(t, o.Units)
			require.Equal(t, reason, o.Error)
		}
	}

	// The guard must also hold when durable repair capture is disabled.
	m.repairs = nil
	require.ErrorContains(t, m.repairIndex(context.Background(), *unsupported.Index), reason)
	require.Len(t, w.created, 1)
	require.Len(t, w.dia.generated, 2)
}

func TestRecoveryMappingDoesNotGenerateUnsupportedIndexDDL(t *testing.T) {
	w := newRepairIndexWriter()
	table := repair.Object{Kind: "table", Table: "T", ColumnMapping: map[string]string{"ID": "id"}}
	o := repair.Object{Kind: "index", Table: "T", Schema: "dst", Index: &source.IndexInfo{
		TableName: "T", IndexName: "IxExpression", Unsupported: "unsupported expression", Columns: nil,
	}}
	got := regenerateRepairCandidate(w, Config{}, table, o, map[string]string{"ID": "renamed"})
	require.True(t, got.RequiresReview)
	require.Empty(t, got.DDL)
	require.Empty(t, got.SuggestedDDL)
	require.Empty(t, got.Units)
	require.Empty(t, w.dia.generated)

	// A manually supplied adaptation remains available for explicit review.
	o.Units = []repair.Unit{{SQL: "CREATE INDEX manual_adaptation ON T (renamed)", OriginalSQL: ""}}
	got = regenerateRepairCandidate(w, Config{}, table, o, map[string]string{"ID": "renamed"})
	require.Equal(t, o.Units, got.Units)
	require.Empty(t, w.dia.generated)
}

func TestLegacyRepairUsesTableIdentityAndNeverImportsSourceDDL(t *testing.T) {
	report := newMigrationReport()
	report.Indexes.Items = []ObjectResult{
		{Name: "IxSame", Table: "TableA", Error: "unsupported", Stage: "unsupported", SourceDDL: "CREATE TABLE TableA (id INT)"},
		{Name: "IxSame", Table: "TableB", Error: "unsupported", Stage: "unsupported", SourceDDL: "CREATE TABLE TableB (id INT)"},
		{Name: "IxNormal", Table: "TableA", Error: "permission denied", DDL: "CREATE INDEX IxNormal ON TableA (id)"},
	}
	objects := LegacyRepairObjects(report, "dst")
	require.Len(t, objects, 3)
	require.NotEqual(t, objects[0].ID, objects[1].ID)
	for i, table := range []string{"TableA", "TableB"} {
		require.Equal(t, table, objects[i].SourceTable)
		require.Equal(t, table, objects[i].Table)
		require.Empty(t, objects[i].DDL)
		require.Empty(t, objects[i].SuggestedDDL)
		require.Empty(t, objects[i].Units)
	}
	require.Len(t, objects[2].Units, 1)
	require.Equal(t, report.Indexes.Items[2].DDL, objects[2].Units[0].SQL)
}

func TestFinishRepairMatchesIndexFailuresWithinTheirTable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		table     string
		duplicate bool
		wantCount int
	}{
		{name: "source table matches", table: "TableB", duplicate: true, wantCount: 2},
		{name: "target table matches", table: "tableb", duplicate: true, wantCount: 2},
		{name: "legacy unique index", wantCount: 1},
		{name: "legacy ambiguous index", duplicate: true, wantCount: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMigrator(&mockReader{}, &mockWriter{})
			b := repair.Object{ID: "b", Kind: "index", Name: "ixsame", SourceName: "IxSame", TargetName: "ixsame", SourceTable: "TableB", Table: "tableb", Status: "pending"}
			m.repairs = &repairRecorder{objects: []repair.Object{b}}
			if tc.duplicate {
				a := b
				a.ID, a.Table, a.SourceTable = "a", "tablea", "TableA"
				m.repairs.objects = append([]repair.Object{a}, m.repairs.objects...)
			}
			report := newMigrationReport()
			report.Indexes.Items = []ObjectResult{{Name: "IxSame", Table: tc.table, Error: "failed on B"}}
			m.finishRepair(context.Background(), &report)
			require.Len(t, report.RepairManifest.Objects, tc.wantCount)
			for _, o := range report.RepairManifest.Objects {
				if o.ID == "a" {
					require.Equal(t, "pending", o.Status)
					require.Empty(t, o.Error)
				}
				if o.ID == "b" {
					if tc.duplicate && tc.table == "" {
						require.Equal(t, "pending", o.Status)
						require.Empty(t, o.Error)
					} else {
						require.Equal(t, "failed", o.Status)
						require.Equal(t, "failed on B", o.Error)
					}
				}
			}
		})
	}
}

func TestSpecializedMigrationUnsupportedIndexesNeverBecomeRepairSQL(t *testing.T) {
	for _, target := range []string{"sqlserver", "oracle"} {
		for _, capture := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/capture=%v", target, capture), func(t *testing.T) {
				var reader *oscarFixtureReader
				var writer *recordingOscarWriter
				var m *Migrator
				if target == "sqlserver" {
					r, w, migrator := sqlServerFixture()
					reader, writer, m = r.oscarFixtureReader, &w.recordingOscarWriter, migrator
				} else {
					r, w, migrator := oracleMigrationFixture()
					reader, writer, m = r.oscarFixtureReader, &w.recordingOscarWriter, migrator
				}
				// Object-only Oracle runs allow unsupported metadata through the
				// preparation phase, so their report must retain the same guard.
				m.cfg.Objects, m.cfg.CaptureRepair = []string{"indexes"}, capture
				reader.indexes = append(reader.indexes, source.IndexInfo{
					TableName: "Orders", IndexName: "IxExpression", Unsupported: "unsupported expression index",
				})
				report := m.Run(context.Background())
				require.Equal(t, 3, report.Indexes.Total)
				require.Equal(t, 2, report.Indexes.Success)
				require.Equal(t, 1, report.Indexes.Failed)
				require.Len(t, report.Indexes.Items, 1)
				item := report.Indexes.Items[0]
				require.Equal(t, "Orders", item.Table)
				require.Equal(t, "unsupported", item.Stage)
				require.Contains(t, strings.ToUpper(item.Name), "IXEXPRESSION")
				require.Empty(t, item.DDL)
				require.Empty(t, item.SuggestedDDL)
				require.NotContains(t, strings.ToUpper(strings.Join(writer.sql, "\n")), "IXEXPRESSION")
				require.Contains(t, strings.ToUpper(strings.Join(writer.sql, "\n")), "IDX_ID")

				var objects []repair.Object
				if capture {
					require.NotNil(t, report.RepairManifest)
					objects = report.RepairManifest.Objects
				} else {
					objects = LegacyRepairObjects(report, m.cfg.TargetSchema)
				}
				failures := 0
				for _, o := range objects {
					if o.Kind != "index" || !strings.Contains(strings.ToUpper(o.Name), "IXEXPRESSION") {
						continue
					}
					failures++
					require.Equal(t, "failed", o.Status)
					require.Equal(t, "Orders", o.SourceTable)
					require.Empty(t, o.DDL)
					require.Empty(t, o.SuggestedDDL)
					require.Empty(t, o.Units)
				}
				require.Equal(t, 1, failures, "failure should merge with its structured repair object")
			})
		}
	}
}
