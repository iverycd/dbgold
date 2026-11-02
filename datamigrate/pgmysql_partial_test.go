package datamigrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type pgPartialWriter struct {
	pgPlanWriter
	fail    string
	failure error
	calls   []string
	copied  []string
}

func (w *pgPartialWriter) CreateTable(_ context.Context, ss []dialect.Statement) error {
	for _, s := range ss {
		w.calls = append(w.calls, s.SQL)
		if w.fail != "" && strings.Contains(s.SQL, w.fail) {
			return w.failure
		}
	}
	return nil
}
func (w *pgPartialWriter) CopyData(_ context.Context, table string, _, _ []string, rows [][]any) error {
	w.copied = append(w.copied, table)
	return nil
}
func (w *pgPartialWriter) CountRows(context.Context, string) (int64, error) { return 0, nil }
func expectPGPartialTarget(mock sqlmock.Sqlmock, n int, ready bool) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
	for i := 0; i < n; i++ {
		mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}))
	}
	if n > 0 {
		mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "def"}))
	}
	if ready {
		mock.ExpectQuery(`SHOW GRANTS`).WillReturnRows(sqlmock.NewRows([]string{"grant"}).AddRow("GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'"))
	}
}
func pgPartialTable(name, typ string) source.PGTable {
	return source.PGTable{Info: source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "id", DataType: typ, IsNullable: true}}}}
}
func TestPGPartialPreflightRunsHealthyTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{pgPartialTable("Bad", "interval"), pgPartialTable("Good", "int4")}}}
	w := &pgPartialWriter{pgPlanWriter: pgPlanWriter{db: db}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "schema_only"})
	expectPGPartialTarget(mock, 1, true)
	expectPGTextRecheck(mock)
	report := m.Run(context.Background())
	require.False(t, report.Preflight.Ready)
	require.True(t, report.Preflight.CanStart)
	require.Equal(t, 1, report.Tables.Success)
	require.Equal(t, 1, report.Tables.Failed)
	require.Contains(t, strings.Join(w.calls, "\n"), "`Good`")
	require.NotContains(t, strings.Join(w.calls, "\n"), "`Bad`")
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestPGRuntimeTableFailureContinuesIndependentObjects(t *testing.T) {
	for _, failure := range []string{"DROP TABLE IF EXISTS `target`.`Bad`", "CREATE TABLE `target`.`Bad`"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			bad, good := pgPartialTable("Bad", "int4"), pgPartialTable("Good", "int4")
			bad.Checks = []source.PGObject{{Kind: "check", Table: "Bad", Name: "bad_check", Definition: "CHECK (id > 0)"}}
			good.Checks = []source.PGObject{{Kind: "check", Table: "Good", Name: "good_check", Definition: "CHECK (id > 0)"}}
			r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{bad, good}, Views: []source.ViewInfo{{ViewName: "BadView", Columns: []source.ColumnInfo{{Name: "id", DataType: "int4"}}, Definition: `SELECT id FROM "Bad"`}, {ViewName: "GoodView", Columns: []source.ColumnInfo{{Name: "id", DataType: "int4"}}, Definition: `SELECT id FROM "Good"`}}}}
			w := &pgPartialWriter{pgPlanWriter: pgPlanWriter{db: db}, fail: failure, failure: errors.New("single table rejected")}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "both", PageSize: 1})
			expectPGPartialTarget(mock, 2, true)
			// Target name lookup for the two planned views.
			// inspectPGTargetNamesAndViews performs these before the dependency-view inventory.
			// Expectations are reordered below to allow the independent catalog queries.
			mock.MatchExpectationsInOrder(false)
			mock.ExpectQuery(`SELECT TABLE_NAME,CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS`).WillReturnRows(sqlmock.NewRows([]string{"table", "constraint"}))
			for i := 0; i < 2; i++ {
				mock.ExpectQuery(`SELECT TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"type"}))
			}
			expectPGTextRecheck(mock)
			report := m.Run(context.Background())
			require.True(t, report.Preflight.CanStart, fmt.Sprint(report.Preflight.Issues))
			require.Equal(t, 1, report.Tables.Success)
			require.Equal(t, 1, report.Tables.Failed)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, 1, report.Constraints.Success)
			require.Equal(t, 1, report.Constraints.Failed)
			require.Equal(t, 1, report.Views.Success)
			require.Equal(t, 1, report.Views.Failed)
			sql := strings.Join(w.calls, "\n")
			require.Contains(t, sql, "good_check")
			require.NotContains(t, sql, "bad_check")
			require.Contains(t, sql, "GoodView")
			require.NotContains(t, sql, "BadView")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestPGTaskFailureStillStopsTableDispatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{pgPartialTable("Bad", "int4"), pgPartialTable("Good", "int4")}}}
	w := &pgPartialWriter{pgPlanWriter: pgPlanWriter{db: db}, fail: "`Bad`", failure: driver.ErrBadConn}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "schema_only"})
	expectPGPartialTarget(mock, 2, true)
	expectPGTextRecheck(mock)
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.NotContains(t, strings.Join(w.calls, "\n"), "`Good`")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGPreflightTargetDependencyClosureAndAllBlocked(t *testing.T) {
	p := &PGMySQLPreflight{blocked: map[ObjectRef]error{}, tables: []source.PGTable{pgPartialTable("Bad", "int4"), pgPartialTable("Parent", "int4"), pgPartialTable("Grandparent", "int4")}}
	p.Target.Database = "target"
	p.targetSnapshot = []target.MySQLForeignKey{{Schema: "target", Table: "Bad", RefSchema: "target", RefTable: "Parent", ConstraintName: "bp"}, {Schema: "target", Table: "Parent", RefSchema: "target", RefTable: "Grandparent", ConstraintName: "pg"}}
	p.add(tableRef("Bad"), "invalid type", true)
	m := NewMigrator(&pgPlanReader{}, &pgPartialWriter{}, &Job{LogCh: make(chan string, 128)}, Config{})
	p.protectTargetDependencies(m)
	p.finish()
	require.False(t, p.Ready)
	require.False(t, p.CanStart)
	require.True(t, p.objectBlocked(tableRef("Parent")))
	require.True(t, p.objectBlocked(tableRef("Grandparent")))
}
func TestPGPartialPreflightBlocksBothNameCollisionOwners(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{pgPartialTable("Same", "int4"), pgPartialTable("SAME", "int4"), pgPartialTable("Good", "int4")}}}
	w := &pgPartialWriter{pgPlanWriter: pgPlanWriter{db: db}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "schema_only", LowerCaseNames: true})
	expectPGPartialTarget(mock, 1, true)
	expectPGTextRecheck(mock)
	report := m.Run(context.Background())
	require.True(t, report.Preflight.CanStart)
	require.Equal(t, 1, report.Tables.Success)
	require.Equal(t, 2, report.Tables.Failed)
	require.Contains(t, strings.Join(w.calls, "\n"), "`good`")
	require.NotContains(t, strings.Join(w.calls, "\n"), "`same`")
	require.NoError(t, mock.ExpectationsWereMet())
}

type pgReplacementWriter struct {
	*pgPartialWriter
	old          []target.MySQLForeignKey
	dependencies []string
}

func (w *pgReplacementWriter) OracleMySQLSchema(context.Context) (string, error) {
	return "target", nil
}
func (w *pgReplacementWriter) OracleMySQLVersion(context.Context) (dialect.MySQLVersion, error) {
	return dialect.MySQLVersion{Major: 8, Minor: 0, Patch: 36}, nil
}
func (w *pgReplacementWriter) OracleMySQLLowerCaseTableNames(context.Context) (int, error) {
	return 0, nil
}
func (w *pgReplacementWriter) OracleMySQLForeignKeys(context.Context) ([]target.MySQLForeignKey, error) {
	return w.old, nil
}
func (w *pgReplacementWriter) ExecuteOracleMySQLDependency(ctx context.Context, sql string) error {
	w.dependencies = append(w.dependencies, sql)
	if err := repair.Observe(ctx, sql, "running", nil); err != nil {
		return err
	}
	return repair.Observe(ctx, sql, "succeeded", nil)
}
func TestPGReplacementTracksOldForeignKeysSeparately(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	bad, good := pgPartialTable("Bad", "int4"), pgPartialTable("Good", "int4")
	bad.Info.Columns[0].IsNullable = false
	bad.Indexes = []source.IndexInfo{{TableName: "Bad", IndexName: "PRIMARY", Columns: []string{"id"}, IsPrimary: true, IsUnique: true, Method: "btree"}}
	good.ForeignKeys = []source.FKInfo{{TableName: "Good", RefTable: "Bad", RefSchema: "public", ConstraintName: "same_name", Columns: []string{"id"}, RefColumns: []string{"id"}, OnDelete: "NO ACTION", OnUpdate: "NO ACTION"}}
	r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{bad, good}}}
	fk := testOracleMySQLTargetFK("target", "Good", "target", "Bad", "same_name")
	w := &pgReplacementWriter{pgPartialWriter: &pgPartialWriter{pgPlanWriter: pgPlanWriter{db: db}, fail: "CREATE TABLE `target`.`Bad`", failure: errors.New("single table rejected")}, old: []target.MySQLForeignKey{fk}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "schema_only"})
	// Exercise the ledger without invoking shared preparation in this focused test.
	m.repairs = &repairRecorder{}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}))
	}
	mock.ExpectQuery(`SELECT TABLE_NAME,CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS`).WillReturnRows(sqlmock.NewRows([]string{"table", "constraint"}))
	mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "def"}))
	mock.ExpectQuery(`SHOW GRANTS`).WillReturnRows(sqlmock.NewRows([]string{"grant"}).AddRow("GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'"))
	expectPGTextRecheck(mock)
	report := m.runPGMySQL(context.Background())
	require.True(t, report.Preflight.CanStart, fmt.Sprint(report.Preflight.Issues))
	require.Equal(t, 1, report.Tables.Success)
	require.Equal(t, 1, report.Tables.Failed)
	require.Len(t, report.TargetDependencies, 1, "plan=%+v state=%+v events=%v issues=%+v", report.Preflight.targetDependencies, m.oracleMySQL, w.dependencies, report.Preflight.Issues)
	require.Equal(t, "blocked", report.TargetDependencies[0].RestoreStatus)
	require.Equal(t, []string{fk.DropSQL}, w.dependencies)
	oldFound, sourceFound := false, false
	for _, o := range m.repairs.objects {
		if o.Kind == "target_dependency" {
			oldFound = true
			require.True(t, o.DiagnosticOnly)
		}
		if o.Kind == "foreign_key" && o.SourceName == "same_name" {
			sourceFound = true
			require.Equal(t, "blocked", o.Status)
		}
	}
	require.True(t, oldFound)
	require.True(t, sourceFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

type pgTaskCountReader struct {
	*pgPlanReader
	counts []string
}

func (r *pgTaskCountReader) CountRows(_ context.Context, table string) (int64, error) {
	r.counts = append(r.counts, table)
	return 0, driver.ErrBadConn
}
func TestPGCountConnectionFailureStopsFurtherValidation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &pgTaskCountReader{pgPlanReader: &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{pgPartialTable("First", "int4"), pgPartialTable("Later", "int4")}}}}
	w := &pgPartialWriter{pgPlanWriter: pgPlanWriter{db: db}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 256)}, Config{Mode: "all", Content: "both", PageSize: 1})
	expectPGPartialTarget(mock, 2, true)
	expectPGTextRecheck(mock)
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, []string{"First"}, r.counts)
	require.NoError(t, mock.ExpectationsWereMet())
}
