package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type gaussFlowWriter struct {
	*partialMigrationWriter
	alterCalls int
}

func (w *gaussFlowWriter) AlterDistribute(context.Context, string, []string) error {
	w.alterCalls++
	return fmt.Errorf("obsolete ALTER must not execute")
}
func (w *gaussFlowWriter) PlanGaussDBIndex(_ context.Context, idx source.IndexInfo) (source.IndexInfo, error) {
	global := !idx.IsPrimary
	idx.GaussDB = &source.GaussDBIndexPlan{Base: source.GaussDBDistribution{Schema: "App", Table: idx.TableName, Mode: "H", Columns: []string{"id"}}, Global: global}
	if global {
		idx.GaussDB.Columns = idx.Columns
	}
	return idx, nil
}

func TestGaussDBMigrationCreatesDistributionBeforeDataAndContinuesFailedTables(t *testing.T) {
	for _, first := range []bool{false, true} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			r, base, m := partialMigrationFixture("mysql", "gaussdb", "create", first)
			for _, name := range r.tables {
				r.ddl[name].Columns = append(r.ddl[name].Columns, source.ColumnInfo{Name: "AutoID", DataType: "bigint"})
				r.indexes = append(r.indexes, source.IndexInfo{TableName: name, IndexName: "Auto_" + name, IsUnique: true, Columns: []string{"AutoID"}})
			}
			w := &gaussFlowWriter{partialMigrationWriter: base}
			m.writer = w
			m.cfg.Distributed = true
			m.cfg.TargetSchema = "App"
			m.cfg.CaptureRepair = true
			report := m.Run(context.Background())
			require.Equal(t, 2, report.Tables.Success)
			require.Equal(t, 1, report.Tables.Failed)
			require.ElementsMatch(t, []string{"a", "z"}, w.copied)
			require.Equal(t, 2, report.PrimaryKeys.Success)
			require.Equal(t, 1, report.PrimaryKeys.Failed)
			require.Zero(t, w.alterCalls)
			for _, ddl := range w.created {
				require.Contains(t, ddl, `DISTRIBUTE BY HASH ("id")`)
			}
			for _, obj := range m.repairs.objects {
				require.NotEqual(t, "distribution", obj.Kind)
				if obj.Index != nil && obj.Status == "succeeded" && (obj.Index.IsPrimary || obj.Index.IsUnique) {
					require.NotNil(t, obj.Index.GaussDB)
					require.Equal(t, dialect.JoinSQL(w.Dialect().IndexStatements("App", *obj.Index)), obj.DDL)
				}
			}
		})
	}
}

func TestGaussDBDistributionSelectionAndNoDestructiveDDLWithoutHashColumn(t *testing.T) {
	r, w, m := partialMigrationFixture("mysql", "gaussdb", "", false)
	m.cfg.Distributed = true
	r.primary = nil
	r.indexes = []source.IndexInfo{{TableName: "A", IndexName: "Z_unique", IsUnique: true, Columns: []string{"Later"}}, {TableName: "A", IndexName: "A_unique", IsUnique: true, Columns: []string{"First"}}}
	r.ddl["A"] = &source.TableDDLInfo{TableName: "A", Columns: []source.ColumnInfo{{Name: "Fallback", DataType: "int"}, {Name: "First", DataType: "bigint"}, {Name: "Later", DataType: "int"}}}
	_, ddl, err := m.buildCreateTableDDL(context.Background(), "A")
	require.NoError(t, err)
	require.Contains(t, ddl, `DISTRIBUTE BY HASH ("first")`)
	r.ddl["Broken"].Columns = []source.ColumnInfo{{Name: "Blob", DataType: "blob"}}
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	for _, attempt := range w.attempts {
		require.False(t, strings.Contains(attempt, `"broken"`))
	}
	require.NotContains(t, w.copied, "broken")
}
