package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type indexReportReader struct {
	mockReader
	indexes         []source.IndexInfo
	indexErr        error
	sourceDDL       string
	sourceErr       error
	definitionReads []string
}

func (r *indexReportReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return r.indexes, r.indexErr
}

func (r *indexReportReader) GetTableSourceDDL(_ context.Context, table string) (string, error) {
	r.definitionReads = append(r.definitionReads, table)
	return r.sourceDDL, r.sourceErr
}

type indexReportWriter struct {
	mockWriter
	indexes []source.IndexInfo
	err     error
}

func (w *indexReportWriter) DBType() string { return "gaussdb" }
func (w *indexReportWriter) Dialect() dialect.Dialect {
	return dialect.NewPostgres("gaussdb")
}
func (w *indexReportWriter) CreateIndex(_ context.Context, index source.IndexInfo) error {
	w.indexes = append(w.indexes, index)
	return w.err
}

func runIndexReport(r *indexReportReader, w *indexReportWriter) MigrationReport {
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType = "gaussdb"
	m.cfg.TargetSchema = "dst"
	m.cfg.LowerCaseNames = true
	m.cfg.Objects = []string{"indexes"}
	report := newMigrationReport()
	m.createPostDDL(context.Background(), &report, []string{"Orders"}, []string{"Orders", "Excluded"})
	return report
}

func TestIndexReportContinuesAfterUnsupportedIndexes(t *testing.T) {
	originalDDL := "CREATE TABLE `Orders` (`ID` int, `Name` varchar(30), KEY `Expr` ((lower(`Name`))))"
	r := &indexReportReader{
		sourceDDL: originalDDL,
		indexes: []source.IndexInfo{
			{TableName: "Orders", IndexName: "Before", Columns: []string{"ID"}},
			{TableName: "Orders", IndexName: "Expr", Unsupported: "函数/表达式索引暂不支持自动转换"},
			{TableName: "Orders", IndexName: "After", Columns: []string{"Name", "ID"}, IsUnique: true},
			{TableName: "Orders", IndexName: "Mixed", Unsupported: "函数/表达式索引暂不支持自动转换"},
			{TableName: "Excluded", IndexName: "Expr", Unsupported: "函数/表达式索引暂不支持自动转换"},
		},
	}
	w := &indexReportWriter{}
	report := runIndexReport(r, w)

	require.Equal(t, 4, report.Indexes.Total)
	require.Equal(t, 2, report.Indexes.Success)
	require.Equal(t, 2, report.Indexes.Failed)
	require.False(t, report.Indexes.TotalUnknown)
	require.True(t, report.HasFailures())
	require.Len(t, w.indexes, 2)
	require.Equal(t, "before", w.indexes[0].IndexName)
	require.Equal(t, "after", w.indexes[1].IndexName)
	require.Equal(t, "orders", w.indexes[1].TableName)
	require.Equal(t, []string{"name", "id"}, w.indexes[1].Columns)
	require.True(t, w.indexes[1].IsUnique)
	require.Equal(t, []string{"Orders"}, r.definitionReads, "read each selected source table definition once")
	for _, item := range report.Indexes.Items {
		require.Equal(t, "Orders", item.Table)
		require.Equal(t, "unsupported", item.Stage)
		require.Empty(t, item.DDL, "source SQL must not be presented as target SQL")
		require.Equal(t, originalDDL, item.SourceDDL)
		require.Contains(t, item.Suggestion, "手工适配")
	}
	require.Equal(t, "Orders", r.indexes[0].TableName, "source metadata must keep its original case")
}

func TestIndexReportMetadataFailureDoesNotInventObjectCount(t *testing.T) {
	r := &indexReportReader{
		indexes:  []source.IndexInfo{{TableName: "Orders", IndexName: "Partial", Columns: []string{"ID"}}},
		indexErr: errors.New("connection closed during index metadata read"),
	}
	w := &indexReportWriter{}
	report := runIndexReport(r, w)
	require.True(t, report.Indexes.TotalUnknown)
	require.Zero(t, report.Indexes.Total)
	require.Zero(t, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.True(t, report.HasFailures())
	require.Len(t, report.Indexes.Items, 1)
	require.Equal(t, "metadata", report.Indexes.Items[0].Stage)
	require.Empty(t, report.Indexes.Items[0].DDL)
	require.Empty(t, w.indexes, "do not execute partial metadata")
	require.Empty(t, r.definitionReads)

	data, err := json.Marshal(report)
	require.NoError(t, err)
	var restored MigrationReport
	require.NoError(t, json.Unmarshal(data, &restored))
	require.True(t, restored.Indexes.TotalUnknown)
	require.True(t, restored.HasFailures())
}

func TestIndexReportExecutionFailureKeepsTargetDDL(t *testing.T) {
	r := &indexReportReader{indexes: []source.IndexInfo{{TableName: "Orders", IndexName: "ByID", Columns: []string{"ID"}}}}
	w := &indexReportWriter{err: errors.New("permission denied")}
	report := runIndexReport(r, w)
	require.Equal(t, 1, report.Indexes.Total)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Len(t, report.Indexes.Items, 1)
	item := report.Indexes.Items[0]
	require.Equal(t, "execute", item.Stage)
	require.Equal(t, "Orders", item.Table)
	require.Equal(t, `CREATE INDEX IF NOT EXISTS "byid" ON "dst"."orders" ("id");`, item.DDL)
	require.Empty(t, item.SourceDDL)
	require.Empty(t, r.definitionReads)
}

func TestIndexReportSourceDefinitionFailureIsIsolated(t *testing.T) {
	r := &indexReportReader{
		sourceErr: errors.New("SHOW CREATE TABLE denied"),
		indexes: []source.IndexInfo{
			{TableName: "Orders", IndexName: "Expr1", Unsupported: "expression"},
			{TableName: "Orders", IndexName: "Expr2", Unsupported: "expression"},
			{TableName: "Orders", IndexName: "Good", Columns: []string{"ID"}},
		},
	}
	w := &indexReportWriter{}
	report := runIndexReport(r, w)
	require.Equal(t, 3, report.Indexes.Total)
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 2, report.Indexes.Failed)
	require.Equal(t, []string{"Orders"}, r.definitionReads)
	require.Len(t, w.indexes, 1)
	for _, item := range report.Indexes.Items {
		require.Equal(t, "unsupported", item.Stage)
		require.Equal(t, "expression", item.Error)
		require.Empty(t, item.DDL)
		require.Empty(t, item.SourceDDL)
		require.Contains(t, item.Suggestion, "SHOW CREATE TABLE denied")
	}
}

func TestIndexReportRunKeepsUnsupportedSourceSQLOutOfRepairUnits(t *testing.T) {
	r := &indexReportReader{
		mockReader: mockReader{
			tables: []string{"Orders"},
			ddl: map[string]*source.TableDDLInfo{
				"Orders": {TableName: "Orders", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}},
			},
		},
		sourceDDL: "CREATE TABLE `Orders` (`ID` int, KEY `Expr` ((abs(`ID`))))",
		indexes: []source.IndexInfo{
			{TableName: "Orders", IndexName: "Expr", Unsupported: "expression index requires manual adaptation"},
			{TableName: "Orders", IndexName: "Good", Columns: []string{"ID"}},
		},
	}
	w := &indexReportWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType, m.cfg.TargetSchema = "gaussdb", "dst"
	m.cfg.LowerCaseNames, m.cfg.CaptureRepair = true, true
	m.cfg.Objects = []string{"indexes"}
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Indexes.Total)
	require.Equal(t, 1, report.Indexes.Success)
	require.Equal(t, 1, report.Indexes.Failed)
	require.Len(t, report.Indexes.Items, 1)
	require.Equal(t, r.sourceDDL, report.Indexes.Items[0].SourceDDL)
	require.NotNil(t, report.RepairManifest)
	indexObjects := 0
	for _, object := range report.RepairManifest.Objects {
		if object.Kind != "index" {
			continue
		}
		indexObjects++
		require.Equal(t, "Orders", object.SourceTable)
		if object.SourceName == "Expr" {
			require.Equal(t, "failed", object.Status)
			require.Empty(t, object.DDL)
			require.Empty(t, object.Units)
		} else {
			require.Equal(t, "Good", object.SourceName)
			require.Equal(t, "succeeded", object.Status)
			require.Len(t, object.Units, 1)
		}
	}
	require.Equal(t, 2, indexObjects, "failures must merge into their original repair objects")
	require.Len(t, w.indexes, 1)
}
