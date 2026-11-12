package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLPGFulltextIndexReportAndRepairManifest(t *testing.T) {
	r := newMySQLDMIndexReader()
	r.ddl["Orders"].Columns = append(r.ddl["Orders"].Columns,
		source.ColumnInfo{Name: "Title", DataType: "varchar", Length: 80},
		source.ColumnInfo{Name: "Body", DataType: "text"})
	r.indexes = []source.IndexInfo{
		{TableName: "Orders", IndexName: "ByText", Method: "FULLTEXT", Columns: []string{"Title", "Body"}},
		{TableName: "Orders", IndexName: "ByTitle", Method: "FULLTEXT", Columns: []string{"Title"}},
	}
	w := &mysqlDMIndexWriter{targetType: "postgres", failures: map[string]error{"bytext": errors.New("access method gin is unsupported")}}
	m, _ := newTestMigrator(r, w)
	m.cfg.Mode, m.cfg.Filter = "include", "Orders"
	m.cfg.Content, m.cfg.Objects = "schema_only", []string{"indexes"}
	m.cfg.TargetDBType, m.cfg.TargetSchema = "postgres", "APP"
	m.cfg.CaptureRepair, m.cfg.LowerCaseNames = true, true

	report := m.Run(context.Background())
	require.Equal(t, 2, report.Indexes.Total)
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Empty(t, w.created)
	require.Len(t, w.indexes, 2)
	require.True(t, w.indexes[0].PGFulltext)
	require.Equal(t, []string{"title", "body"}, w.indexes[0].Columns)
	require.Equal(t, []string{"Title", "Body"}, r.indexes[0].Columns)
	require.Len(t, report.Warnings, 1)
	require.Contains(t, report.Warnings[0].Reason, "MATCH ... AGAINST")
	require.Contains(t, report.Warnings[0].Reason, "ByTitle")

	require.Len(t, report.Indexes.Items, 1)
	item := report.Indexes.Items[0]
	require.Equal(t, "execute", item.Stage)
	require.Contains(t, item.Error, "access method gin is unsupported")
	require.Contains(t, item.DDL, `CREATE INDEX "bytext" ON "APP"."orders" USING gin`)
	require.NotNil(t, report.RepairManifest)
	for _, object := range report.RepairManifest.Objects {
		if object.SourceName == "ByText" {
			require.NotNil(t, object.Index)
			require.True(t, object.Index.PGFulltext)
			require.Len(t, object.Units, 1)
			require.Equal(t, item.DDL, object.Units[0].SQL)
			require.Equal(t, "failed", object.Units[0].Status)
			return
		}
	}
	t.Fatal("missing failed GIN index in repair manifest")
}
