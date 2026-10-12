package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type repairReader struct {
	*dmFixtureReader
	kind     string
	primary  []source.IndexInfo
	comments []source.CommentInfo
	mu       sync.Mutex
	readKeys [][]string
}

func (r *repairReader) DBType() string { return r.kind }
func (r *repairReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return r.primary, nil
}
func (r *repairReader) GetPrimaryKey(_ context.Context, table string) ([]string, error) {
	return []string{"RowId"}, nil
}
func (r *repairReader) GetComments(context.Context) ([]source.CommentInfo, error) {
	return r.comments, nil
}
func (r *repairReader) CountRows(context.Context, string) (int64, error) { return 3, nil }
func (r *repairReader) ReadPage(_ context.Context, table string, keys []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	r.mu.Lock()
	r.readKeys = append(r.readKeys, append([]string(nil), keys...))
	r.mu.Unlock()
	cols, types := []string{}, []string{}
	for _, c := range r.ddl[table].Columns {
		cols = append(cols, c.Name)
		types = append(types, "INT")
	}
	rows := [][]interface{}{}
	for i := offset; i < 3 && i < offset+limit; i++ {
		row := make([]interface{}, len(cols))
		for j := range row {
			row[j] = i + 1
		}
		rows = append(rows, row)
	}
	return cols, types, rows, nil
}

type repairWriter struct {
	*dmFixtureWriter
	mu            sync.Mutex
	columns       map[string][][]string
	sequences     []source.SequenceInfo
	comments      []source.CommentInfo
	validated     []*source.TableDDLInfo
	validationErr error
	strictInfo    *source.TableDDLInfo
	boundsColumn  string
}

func (w *repairWriter) CountRows(context.Context, string) (int64, error) { return 3, nil }
func (w *repairWriter) CopyData(_ context.Context, table string, cols, _ []string, _ [][]interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.copyDataFail {
		return fmt.Errorf("copy failed")
	}
	if w.columns == nil {
		w.columns = map[string][][]string{}
	}
	w.columns[table] = append(w.columns[table], [][]string{append([]string(nil), cols...)}...)
	return nil
}
func (w *repairWriter) CreateSequence(_ context.Context, s source.SequenceInfo, _ string) error {
	w.sequences = append(w.sequences, s)
	return nil
}
func (w *repairWriter) CreateComment(_ context.Context, c source.CommentInfo) error {
	w.comments = append(w.comments, c)
	return nil
}
func (w *repairWriter) ValidateDamengColumns(_ context.Context, info *source.TableDDLInfo) error {
	w.validated = append(w.validated, info)
	return w.validationErr
}
func (w *repairWriter) ValidateSQLServerDamengExisting(_ context.Context, info *source.TableDDLInfo, _ dialect.TypeOpt) error {
	w.strictInfo = info
	return nil
}
func (w *repairWriter) SQLServerDamengIdentityBounds(_ context.Context, _, column string) (*int64, *int64, error) {
	w.boundsColumn = column
	return nil, nil, nil
}
func repairFixture(kind string) (*repairReader, *repairWriter, *Migrator) {
	r, w := dmFixture()
	r.tables = []string{"T", "U"}
	def := "'ROWID value'"
	r.ddl = map[string]*source.TableDDLInfo{
		"T": {TableName: "T", Columns: []source.ColumnInfo{{Name: "RowId", DataType: "int"}, {Name: "ROW_ID", DataType: "int"}, {Name: "Txt", DataType: "varchar", Length: 100, IsNullable: true, Default: &def}}},
		"U": {TableName: "U", Columns: []source.ColumnInfo{{Name: "RowId", DataType: "int"}}},
	}
	r.indexes = []source.IndexInfo{{TableName: "T", IndexName: "IX", Columns: []string{"RowId"}}}
	r.fks = []source.FKInfo{{TableName: "T", ConstraintName: "FK", Columns: []string{"RowId"}, RefTable: "U", RefColumns: []string{"RowId"}}}
	rr := &repairReader{dmFixtureReader: r, kind: kind, primary: []source.IndexInfo{{TableName: "T", IsPrimary: true, Columns: []string{"RowId"}}}, comments: []source.CommentInfo{{TableName: "T", ColumnName: "RowId", Comment: "ROWID literal"}}}
	ww := &repairWriter{dmFixtureWriter: w}
	m := NewMigrator(rr, ww, &Job{LogCh: make(chan string, 4096)}, Config{TargetDBType: "dameng", TargetSchema: "APP", Content: "both", Mode: "all", PageSize: 1, MaxParallel: 1})
	return rr, ww, m
}

func TestDamengRepairAllWritePaths(t *testing.T) {
	for _, kind := range []string{"mysql", "postgres", "sqlserver"} {
		for _, parallel := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/%d", kind, parallel), func(t *testing.T) {
				r, w, m := repairFixture(kind)
				m.cfg.IntraTableParallel = parallel
				report := m.Run(context.Background())
				require.False(t, report.HasFailures(), "%+v", report)
				require.Len(t, report.AutoRepairs, 2)
				require.Contains(t, w.created[0], `"ROW_ID_2" INT`)
				require.Contains(t, w.created[0], `'ROWID value'`)
				for table, pages := range w.columns {
					for _, cols := range pages {
						want := "ROW_ID"
						if strings.EqualFold(table, "T") {
							want = "ROW_ID_2"
						}
						require.Equal(t, want, cols[0])
					}
				}
				for _, keys := range r.readKeys {
					if len(keys) > 0 {
						require.Equal(t, []string{"RowId"}, keys)
					}
				}
				require.Equal(t, "RowId", r.ddl["T"].Columns[0].Name)
				require.Len(t, w.indexes, 2)
				for _, idx := range w.indexes {
					require.Equal(t, []string{"ROW_ID_2"}, idx.Columns)
				}
				require.Equal(t, []string{"ROW_ID_2"}, w.fks[0].Columns)
				require.Equal(t, []string{"ROW_ID"}, w.fks[0].RefColumns)
				require.Equal(t, "ROW_ID_2", w.comments[0].ColumnName)
			})
		}
	}
}

func TestDamengRepairReuseAndFailureReporting(t *testing.T) {
	for _, mode := range []string{"data_only", "objects"} {
		t.Run(mode, func(t *testing.T) {
			r, w, m := repairFixture("sqlserver")
			m.cfg.Content = mode
			if mode == "objects" {
				m.cfg.Objects = []string{"indexes", "sequences"}
			}
			r.ddl["T"].Columns[0].Extra = "auto_increment"
			id := &source.IdentityInfo{Seed: 1, Increment: 1}
			r.ddl["T"].Columns[0].IdentityInfo = id
			r.sequences = []source.SequenceInfo{{TableName: "T", ColumnName: "RowId", IdentityInfo: id}}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Empty(t, w.created)
			require.Equal(t, "ROW_ID_2", w.validated[0].Columns[0].Name)
			require.Equal(t, "ROW_ID_2", w.sequences[0].ColumnName)
			require.Equal(t, "ROW_ID_2", w.boundsColumn)
			for _, repair := range report.AutoRepairs {
				require.Equal(t, "reused", repair.Status)
			}
		})
	}
	for _, failure := range []string{"create", "data", "existing"} {
		_, w, m := repairFixture("mysql")
		switch failure {
		case "create":
			w.createTableFail = true
		case "data":
			w.copyDataFail = true
		case "existing":
			m.cfg.Content = "data_only"
			w.validationErr = fmt.Errorf("missing mapped column")
		}
		report := m.Run(context.Background())
		require.True(t, report.HasFailures())
		if failure == "data" {
			require.Len(t, report.AutoRepairs, 2)
		} else {
			require.Empty(t, report.AutoRepairs)
			require.Empty(t, w.columns)
			require.Empty(t, w.indexes)
			require.Empty(t, w.fks)
		}
	}
}

func TestDamengRepairViewsAndDependencies(t *testing.T) {
	r, w, m := repairFixture("sqlserver")
	r.views = []source.ViewInfo{
		{ViewName: "Child", Definition: "CREATE VIEW dbo.Child AS SELECT v.RowId AS ID FROM dbo.Parent v"},
		{ViewName: "Parent", Definition: "CREATE VIEW dbo.Parent AS SELECT t.* FROM dbo.T t"},
		{ViewName: "Bad", Definition: "CREATE VIEW dbo.Bad AS SELECT TOP 1 RowId FROM dbo.T"},
		{ViewName: "BadChild", Definition: "CREATE VIEW dbo.BadChild AS SELECT RowId FROM dbo.Bad"},
	}
	report := m.Run(context.Background())
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 2, report.Views.Success)
	require.Equal(t, 2, report.Views.Failed)
	require.Len(t, report.AutoRepairs, 2)
	require.Equal(t, "Parent", w.views[0].ViewName)
	require.Contains(t, w.views[0].Definition, `"T"."ROW_ID_2"`)
	require.Contains(t, w.views[1].Definition, `"V"."ROW_ID_2" AS "ID"`)
	// Fresh migrator must discover/reuse mappings, without re-creating any table.
	w.views = nil
	vm := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, m.cfg)
	results := vm.MigrateViews(context.Background(), []string{"Child"})
	require.Len(t, results, 1)
	require.Empty(t, results[0].Error)
	require.Len(t, w.views, 1)
	require.Equal(t, "Child", w.views[0].ViewName)
	require.Contains(t, w.views[0].Definition, `"V"."ROW_ID_2"`)
}

func TestDamengObjectOnlyExternalReference(t *testing.T) {
	_, w, m := repairFixture("sqlserver")
	m.cfg.Objects = []string{"foreign_keys"}
	m.cfg.TableNames = []string{"T"}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Len(t, w.fks, 1)
	require.Equal(t, []string{"ROW_ID"}, w.fks[0].RefColumns)
	require.Len(t, w.validated, 2) // T and existing referenced U
}

func TestDamengGenericViewReuseRejectsMissingTargetMapping(t *testing.T) {
	for _, kind := range []string{"mysql", "postgres"} {
		r, w, m := repairFixture(kind)
		r.views = []source.ViewInfo{{ViewName: "V", Definition: "SELECT t.RowId AS SavedId FROM T t"}}
		report := m.Run(context.Background())
		require.Zero(t, report.Views.Failed)
		require.Contains(t, w.views[0].Definition, `"T"."ROW_ID_2" AS "SAVEDID"`)
		w.views = nil
		w.validationErr = fmt.Errorf("目标缺少 ROW_ID_2")
		vm := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, m.cfg)
		out := vm.MigrateViews(context.Background(), []string{"V"})
		require.Len(t, out, 1)
		require.Contains(t, out[0].Error, "目标缺少 ROW_ID_2")
		require.Empty(t, w.views)
	}
}

func TestDamengRepairStillRejectsUnsafeStructureBeforeWrites(t *testing.T) {
	for _, failure := range []string{"column collision", "type", "table collision"} {
		r, w, m := repairFixture("sqlserver")
		switch failure {
		case "column collision":
			r.ddl["T"].Columns = append(r.ddl["T"].Columns, source.ColumnInfo{Name: "ROWID", DataType: "int"})
		case "type":
			r.ddl["T"].Columns[0].DataType = "geometry"
		case "table collision":
			r.tables = append(r.tables, "t")
			r.ddl["t"] = r.ddl["T"]
		}
		report := m.Run(context.Background())
		require.True(t, report.HasFailures())
		require.Empty(t, w.created)
		require.Empty(t, report.AutoRepairs)
	}
}
