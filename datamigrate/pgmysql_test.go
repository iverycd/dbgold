package datamigrate

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"strings"
	"testing"
)

type pgPlanReader struct {
	source.Reader
	snapshot *source.PGSnapshot
	err      error
	rows     [][]any
}

func (r *pgPlanReader) DBType() string { return "postgres" }
func (r *pgPlanReader) ReadPGSnapshot(context.Context) (*source.PGSnapshot, error) {
	return r.snapshot, r.err
}
func (r *pgPlanReader) ReadPage(_ context.Context, _ string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	if offset > 0 {
		return []string{"id"}, nil, nil, nil
	}
	return []string{"id"}, nil, r.rows, nil
}
func (r *pgPlanReader) CountRows(context.Context, string) (int64, error) {
	return int64(len(r.rows)), nil
}

type pgPlanWriter struct {
	target.Writer
	db        *sql.DB
	mutations int
}

func (w *pgPlanWriter) DBType() string        { return "mysql" }
func (w *pgPlanWriter) InspectionDB() *sql.DB { return w.db }
func (w *pgPlanWriter) InspectMySQL(context.Context) (target.MySQLProfile, error) {
	v, _ := dialect.ParseMySQLVersion("8.0.36")
	return target.MySQLProfile{Version: "8.0.36", Parsed: v, Database: "target", TargetCharset: "utf8mb4", CharsetMaxBytes: 4, CollationPadSpace: true, TargetCollation: "utf8mb4_bin", PageSize: 16384, LargePrefix: true}, nil
}
func (w *pgPlanWriter) CreateTable(context.Context, []dialect.Statement) error {
	w.mutations++
	return nil
}
func (w *pgPlanWriter) CopyData(context.Context, string, []string, []string, [][]any) error {
	w.mutations++
	return nil
}
func TestPGPreflightBlocksBeforeAnyMutation(t *testing.T) {
	for _, dt := range []string{"_int4", "numeric", "interval"} {
		t.Run(dt, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "id", DataType: dt}}}}}}}
			w := &pgPlanWriter{db: db}
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
			mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
			mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t"}))
			mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "def"}))
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", TargetDBType: "mysql", Content: "both"})
			report := m.Run(context.Background())
			if !report.HasFailures() || report.Preflight.Ready || w.mutations != 0 {
				t.Fatalf("unsafe result: %+v writes=%d", report, w.mutations)
			}
			if e := mock.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestPGViewRestrictedTranslation(t *testing.T) {
	cols := []source.ColumnInfo{{Name: "Id"}, {Name: "Text"}}
	relations := map[string][]source.ColumnInfo{"Mixed": cols}
	v := source.ViewInfo{ViewName: "View", Columns: []source.ColumnInfo{{Name: "Id"}}, Definition: `SELECT m."Id" AS "Id" FROM "public"."Mixed" m WHERE m."Text" = 'a::b'::text`}
	sql, deps, e := translatePGView(v, "public", "dst", relations, strings.ToLower)
	if e != nil || len(deps) != 1 || !strings.Contains(sql, "`dst`.`mixed`") || !strings.Contains(sql, "'a::b'") {
		t.Fatal(sql, deps, e)
	}
	for _, def := range []string{`SELECT count(*) FROM "Mixed"`, `WITH x AS (SELECT 1) SELECT * FROM x`, `SELECT "Id"::varchar FROM "Mixed"`, `SELECT "Id" FROM other."Mixed"`} {
		v.Definition = def
		if _, _, e = translatePGView(v, "public", "dst", relations, strings.ToLower); e == nil {
			t.Fatal(def)
		}
	}
}
func TestMySQLCommentPreservesColumnAttributes(t *testing.T) {
	ddl := "CREATE TABLE `t` (\n `id` bigint NOT NULL AUTO_INCREMENT COMMENT 'old',\n `value` varchar(30) NOT NULL DEFAULT 'a\\\\b' COLLATE utf8mb4_bin COMMENT 'old'\n) ENGINE=InnoDB"
	def, e := mysqlColumnDefinition(ddl, "value")
	if e != nil {
		t.Fatal(e)
	}
	def, e = replaceMySQLColumnComment(def, "it's new")
	if e != nil || !strings.Contains(def, `DEFAULT 'a\b'`) || !strings.Contains(def, "COLLATE utf8mb4_bin") || !strings.Contains(def, "COMMENT 'it''s new'") {
		t.Fatal(def, e)
	}
}
func TestPGKeyPreservesServerWeightBytes(t *testing.T) {
	cols := []source.ColumnInfo{{Name: "c", DataType: "varchar"}}
	a, _, _ := pgKey([]any{[]byte("x")}, cols, []string{"c"})
	b, _, _ := pgKey([]any{[]byte("x ")}, cols, []string{"c"})
	if a == b {
		t.Fatal("server weights must not be trimmed as text")
	}
	_, valid, _ := pgKey([]any{nil}, cols, []string{"c"})
	if valid {
		t.Fatal("NULL key must be exempt")
	}
}

func expectPGEmptyTarget(mock sqlmock.Sqlmock, grant string) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
	mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}))
	mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "def"}))
	mock.ExpectQuery(`SHOW GRANTS`).WillReturnRows(sqlmock.NewRows([]string{"grant"}).AddRow(grant))
}
func TestPGStartupRechecksMetadataAndPrivileges(t *testing.T) {
	for _, grant := range []string{"GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'", "GRANT SELECT, DROP ON *.* TO 'test'@'localhost'"} {
		t.Run(grant, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "id", DataType: "int4"}}}}}}}
			w := &pgPlanWriter{db: db}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "schema_only"})
			expectPGEmptyTarget(mock, grant)
			preview := m.PreflightPGMySQL(context.Background())
			all := strings.Contains(grant, "ALL PRIVILEGES")
			if preview.Ready != all {
				t.Fatalf("unexpected preview: %+v", preview)
			}
			// A valid preview cannot authorize a later run with failed extraction.
			r.err = errors.New("permission denied for source catalog")
			report := m.Run(context.Background())
			if !report.HasFailures() || w.mutations != 0 {
				t.Fatalf("unsafe run: %+v writes=%d", report, w.mutations)
			}
			if e := mock.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestPGPredicateDoesNotEraseTemporalCast(t *testing.T) {
	table := source.TableDDLInfo{Columns: []source.ColumnInfo{{Name: "at", DataType: "timestamptz"}}}
	if _, err := translatePGCheck(`CHECK (at > '2024-01-01 01:00:00+08'::timestamptz)`, table, func(s string) string { return s }); err == nil {
		t.Fatal("time-zone cast silently erased")
	}
}

func TestPGReportCountsEveryFailureCategoryAndValidation(t *testing.T) {
	for _, kind := range []string{"table", "index", "primary_key", "sequence", "foreign_key", "comment", "trigger", "view"} {
		r := newMigrationReport()
		pgCategory(&r, kind).Failed = 1
		if !r.HasFailures() {
			t.Fatal("failure ignored", kind)
		}
	}
	r := newMigrationReport()
	r.RowCounts = []TableRowCount{{Table: "T", Src: 2, Dst: 1, Match: false}}
	if !r.HasFailures() {
		t.Fatal("row mismatch ignored")
	}
}
