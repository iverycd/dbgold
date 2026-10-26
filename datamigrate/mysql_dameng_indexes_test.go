package datamigrate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type mysqlDMIndexReader struct {
	indexReportReader
	sourceType  string
	primaryKeys []source.IndexInfo
}

func (r *mysqlDMIndexReader) DBType() string { return r.sourceType }
func (r *mysqlDMIndexReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return r.primaryKeys, nil
}

type mysqlDMIndexWriter struct {
	mockWriter
	targetType string
	indexes    []source.IndexInfo
	reused     map[string]bool
	failures   map[string]error
}

func (w *mysqlDMIndexWriter) DBType() string { return w.targetType }
func (w *mysqlDMIndexWriter) Dialect() dialect.Dialect {
	if w.targetType == "dameng" {
		return dialect.NewDaMeng()
	}
	return dialect.NewPostgres(w.targetType)
}
func (w *mysqlDMIndexWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	w.indexes = append(w.indexes, idx)
	sql := dialect.JoinSQL(w.Dialect().IndexStatements("APP", idx))
	if err := repair.Observe(ctx, sql, "running", nil); err != nil {
		return err
	}
	if err := w.failures[idx.IndexName]; err != nil {
		return errors.Join(err, repair.Observe(ctx, sql, "failed", err))
	}
	status := "succeeded"
	if w.reused[idx.IndexName] {
		status = "reused"
	}
	return repair.Observe(ctx, sql, status, nil)
}

func newMySQLDMIndexReader() *mysqlDMIndexReader {
	return &mysqlDMIndexReader{
		sourceType: "mysql",
		indexReportReader: indexReportReader{mockReader: mockReader{
			tables: []string{"Orders"},
			ddl: map[string]*source.TableDDLInfo{"Orders": {
				TableName: "Orders",
				Columns: []source.ColumnInfo{
					{Name: "ROW_ID", DataType: "int"},
					{Name: "Code", DataType: "int"},
				},
			}},
		}},
		primaryKeys: []source.IndexInfo{{TableName: "Orders", IndexName: "PRIMARY", Columns: []string{"ROW_ID"}, IsPrimary: true, IsUnique: true}},
	}
}

func TestMySQLDamengIndexCreationOrderPreservesOtherPairs(t *testing.T) {
	for _, tc := range []struct {
		sourceType, targetType string
		want                   []string
	}{
		{"mysql", "dameng", []string{"PRIMARY", "UniqueB", "UniqueA", "NormalA", "NormalB"}},
		{"mysql", "postgres", []string{"PRIMARY", "NormalA", "UniqueB", "NormalB", "UniqueA"}},
		{"sqlserver", "dameng", []string{"PRIMARY", "NormalA", "UniqueB", "NormalB", "UniqueA"}},
	} {
		t.Run(tc.sourceType+"/"+tc.targetType, func(t *testing.T) {
			r := newMySQLDMIndexReader()
			r.sourceType = tc.sourceType
			r.indexes = []source.IndexInfo{
				{TableName: "Orders", IndexName: "NormalA", Columns: []string{"ROW_ID"}},
				{TableName: "Orders", IndexName: "UniqueB", Columns: []string{"Code"}, IsUnique: true},
				{TableName: "Orders", IndexName: "NormalB", Columns: []string{"Code"}},
				{TableName: "Orders", IndexName: "UniqueA", Columns: []string{"ROW_ID"}, IsUnique: true},
				{TableName: "Excluded", IndexName: "ExcludedUnique", Columns: []string{"ROW_ID"}, IsUnique: true},
			}
			original := slices.Clone(r.indexes)
			w := &mysqlDMIndexWriter{targetType: tc.targetType}
			m, _ := newTestMigrator(r, w)
			m.cfg.TargetSchema, m.cfg.Objects = "APP", []string{"primary_keys", "indexes"}
			report := newMigrationReport()
			m.createPostDDL(context.Background(), &report, []string{"Orders"}, []string{"Orders", "Excluded"})

			var names []string
			for _, idx := range w.indexes {
				names = append(names, idx.IndexName)
			}
			want := slices.Clone(tc.want)
			if tc.sourceType == "sqlserver" {
				for i := range want {
					want[i] = strings.ToUpper(want[i])
				}
			}
			require.Equal(t, want, names)
			require.Equal(t, original, r.indexes, "sorting must not mutate the reader's metadata")
			require.Equal(t, 1, report.PrimaryKeys.Success)
			require.Equal(t, 4, report.Indexes.Total)
			require.Equal(t, 4, report.Indexes.Success)
			require.False(t, report.HasFailures())
		})
	}
}

func TestMySQLDamengReusedIndexKeepsOneSuccessfulManifestUnit(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "without capture", true: "with capture"}[capture], func(t *testing.T) {
			r := newMySQLDMIndexReader()
			r.indexes = []source.IndexInfo{
				{TableName: "Orders", IndexName: "ByRowID", Columns: []string{"ROW_ID"}},
				{TableName: "Orders", IndexName: "UniqueCode", Columns: []string{"Code"}, IsUnique: true},
			}
			w := &mysqlDMIndexWriter{targetType: "dameng", reused: map[string]bool{"ByRowID": true}}
			m, job := newTestMigrator(r, w)
			m.cfg.TargetDBType, m.cfg.TargetSchema = "dameng", "APP"
			m.cfg.CaptureRepair, m.cfg.Objects = capture, []string{"primary_keys", "indexes"}
			report := m.Run(context.Background())

			require.False(t, report.HasFailures(), "%+v", report)
			require.Equal(t, 2, report.Indexes.Total)
			require.Equal(t, 2, report.Indexes.Success)
			require.Zero(t, report.Indexes.Failed)
			require.Empty(t, report.Indexes.Items)
			if !capture {
				require.Nil(t, report.RepairManifest)
				return
			}
			require.NotNil(t, report.RepairManifest)
			found := false
			for _, obj := range report.RepairManifest.Objects {
				if obj.Kind != "index" || obj.SourceName != "ByRowID" {
					continue
				}
				found = true
				require.Equal(t, "succeeded", obj.Status)
				require.Empty(t, obj.Error)
				require.Len(t, obj.Units, 1, "reused must not append a second execution unit")
				require.Equal(t, "succeeded", obj.Units[0].Status)
				require.False(t, obj.Units[0].Selected)
			}
			require.True(t, found)
			require.Contains(t, strings.Join(drainLogs(job), "\n"), "已核验并复用目标对象")
		})
	}
}

func TestMySQLDamengFailedPrimaryKeyDoesNotSkipOrdinaryIndex(t *testing.T) {
	r := newMySQLDMIndexReader()
	r.indexes = []source.IndexInfo{{TableName: "Orders", IndexName: "ByRowID", Columns: []string{"ROW_ID"}}}
	w := &mysqlDMIndexWriter{targetType: "dameng", failures: map[string]error{"PRIMARY": errors.New("duplicate primary key values")}}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetSchema, m.cfg.Objects = "APP", []string{"primary_keys", "indexes"}
	report := newMigrationReport()
	m.createPostDDL(context.Background(), &report, []string{"Orders"}, []string{"Orders"})

	require.Equal(t, 1, report.PrimaryKeys.Failed)
	require.Equal(t, 1, report.Indexes.Success)
	require.Len(t, w.indexes, 2, "ordinary index must still be attempted against the actual target")
	require.Equal(t, "ByRowID", w.indexes[1].IndexName)
}

type mysqlDMIndexRunWriter struct {
	mysqlDMIndexWriter
	runEvidence map[string]bool
	resets      int
}

func (w *mysqlDMIndexRunWriter) SetSourceType(string) {
	w.resets++
	w.runEvidence = map[string]bool{}
}

func (w *mysqlDMIndexRunWriter) CreateIndex(ctx context.Context, idx source.IndexInfo) error {
	if w.runEvidence[idx.IndexName] {
		return errors.New("stale index coverage carried over from previous run")
	}
	w.runEvidence[idx.IndexName] = true
	return w.mysqlDMIndexWriter.CreateIndex(ctx, idx)
}

func TestMySQLDamengRepeatedObjectRunsResetIndexEvidence(t *testing.T) {
	r := newMySQLDMIndexReader()
	r.indexes = []source.IndexInfo{{TableName: "Orders", IndexName: "ByRowID", Columns: []string{"ROW_ID"}}}
	w := &mysqlDMIndexRunWriter{mysqlDMIndexWriter: mysqlDMIndexWriter{targetType: "dameng"}}
	m, _ := newTestMigrator(r, w)
	m.cfg.TargetDBType, m.cfg.TargetSchema = "dameng", "APP"
	m.cfg.Objects = []string{"indexes"}
	for range 2 {
		report := m.Run(context.Background())
		require.False(t, report.HasFailures(), "%+v", report)
		require.Equal(t, 1, report.Indexes.Success)
	}
	require.Equal(t, 3, w.resets, "constructor plus each Run must establish a fresh evidence scope")
	require.Empty(t, w.created, "object-only runs do not reset evidence through CreateTable")
	require.Len(t, w.indexes, 2)
}
