package datamigrate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
)

// Recover source-equivalent metadata from the user's complete planned MySQL
// DDL. No indexes were supplied; index protection is tested independently below.
func wideFixture(t *testing.T, name string) source.PGTable {
	t.Helper()
	raw, err := os.ReadFile("testdata/pgmysql_wide/" + name + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	node, err := parser.New().ParseOneStmt(string(raw), "", "")
	if err != nil {
		t.Fatal(err)
	}
	create := node.(*ast.CreateTableStmt)
	result := source.PGTable{Info: source.TableDDLInfo{TableName: name}}
	for _, col := range create.Cols {
		c := source.ColumnInfo{Name: col.Name.Name.O, IsNullable: true}
		switch col.Tp.GetType() {
		case mysql.TypeVarchar:
			c.DataType, c.Length = "varchar", int64(col.Tp.GetFlen())
		case mysql.TypeLong:
			c.DataType = "int4"
		case mysql.TypeDatetime:
			c.DataType, c.DateTimePrecision = "timestamp", int64(col.Tp.GetDecimal())
		case mysql.TypeDate:
			c.DataType = "date"
		case mysql.TypeNewDecimal:
			c.DataType, c.Precision, c.Scale = "numeric", int64(col.Tp.GetFlen()), int64(col.Tp.GetDecimal())
		case mysql.TypeLongBlob:
			c.DataType = "text"
		default:
			t.Fatalf("unhandled type %v", col.Tp)
		}
		for _, opt := range col.Options {
			if opt.Tp == ast.ColumnOptionNotNull {
				c.IsNullable = false
			}
			if opt.Tp == ast.ColumnOptionComment {
				c.Comment = "测试时间"
			}
		}
		result.Info.Columns = append(result.Info.Columns, c)
	}
	return result
}

func TestVastbaseWideTablePlans(t *testing.T) {
	for _, tc := range []struct {
		name, mapped string
		count        int
	}{
		{"kgyjs_sqzb", "jsgm", 126}, {"projectbiaoduan", "jsgmjnr", 197},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := wideFixture(t, tc.name)
			if len(table.Info.Columns) != tc.count {
				t.Fatalf("columns=%d", len(table.Info.Columns))
			}
			db, mock, _ := sqlmock.New()
			defer db.Close()
			r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{table}}}
			w := &pgPlanWriter{db: db}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "schema_only"})
			expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			p := m.PreflightPGMySQL(context.Background())
			if !p.Ready {
				t.Fatalf("issues=%+v", p.Issues)
			}
			var changed []string
			for i, c := range p.tables[0].Info.Columns {
				if table.Info.Columns[i].PGMySQLLongText {
					t.Fatal("mutated source snapshot")
				}
				if !c.PGMySQLLongText {
					continue
				}
				changed = append(changed, c.Name)
				val := strings.Repeat("中", int(c.Length))
				if got, err := valueconv.PGMySQLValue(val, c); err != nil || got != val {
					t.Fatal("text lost", err)
				}
				if _, err := valueconv.PGMySQLValue(val+"中", c); err == nil {
					t.Fatal("length limit lost")
				}
			}
			if strings.Join(changed, ",") != tc.mapped {
				t.Fatalf("changed=%v", changed)
			}
			for _, o := range p.Objects {
				if o.Stage != "table" {
					continue
				}
				ddl := o.Statements[1].SQL
				if !strings.Contains(ddl, "`"+tc.mapped+"` LONGTEXT") || !strings.Contains(ddl, "CHECK (CHAR_LENGTH(`"+tc.mapped+"`) <= 4000)") {
					t.Fatal(ddl)
				}
				if _, err := parser.New().ParseOneStmt(ddl, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%d columns; issues=%+v", len(table.Info.Columns), p.Issues)
			if w.mutations != 0 {
				t.Fatal("preflight wrote to target")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPGWideTableMappingGuards(t *testing.T) {
	for _, scenario := range []string{"plain", "index", "incoming_fk", "outgoing_fk", "unknown_index", "all_keys", "old_mysql", "latin1", "bad_default"} {
		t.Run(scenario, func(t *testing.T) {
			table := source.PGTable{Info: source.TableDDLInfo{TableName: "T"}}
			for i := 0; i < 5; i++ {
				table.Info.Columns = append(table.Info.Columns, source.ColumnInfo{Name: fmt.Sprintf("c%d", i), DataType: "varchar", Length: 4000, IsNullable: true})
			}
			profile, _ := (&pgPlanWriter{}).InspectMySQL(context.Background())
			snap := &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}}
			p := &PGMySQLPreflight{Source: snap.Profile, Target: profile, snapshot: snap}
			switch scenario {
			case "index":
				table.Indexes = []source.IndexInfo{{Columns: []string{"c0"}}}
			case "unknown_index":
				table.Indexes = []source.IndexInfo{{Unsupported: "expression index"}}
			case "all_keys":
				table.Indexes = []source.IndexInfo{{Columns: []string{"c0", "c1", "c2", "c3", "c4"}}}
			case "incoming_fk":
				snap.Tables = []source.PGTable{{ForeignKeys: []source.FKInfo{{TableName: "child", RefSchema: "public", RefTable: "T", RefColumns: []string{"c0"}}}}}
			case "outgoing_fk":
				snap.Tables = []source.PGTable{{ForeignKeys: []source.FKInfo{{TableName: "T", Columns: []string{"c0"}}}}}
			case "old_mysql":
				p.Target.Parsed = dialect.MySQLVersion{Major: 5, Minor: 7, Patch: 44}
			case "latin1":
				p.Target.TargetCharset, p.Target.TargetCollation, p.Target.CharsetMaxBytes = "latin1", "latin1_bin", 1
			case "bad_default":
				def := "untrusted_function()"
				table.Info.Columns[0].Default = &def
			}
			(&Migrator{}).fitPGMySQLRow(p, &table)
			var changed []string
			for _, c := range table.Info.Columns {
				if c.PGMySQLLongText {
					changed = append(changed, c.Name)
				}
			}
			want := "c0"
			switch scenario {
			case "index", "incoming_fk", "outgoing_fk", "bad_default":
				want = "c1"
			case "unknown_index", "all_keys", "old_mysql", "latin1":
				want = ""
			}
			if strings.Join(changed, ",") != want {
				t.Fatalf("changed=%v want=%s", changed, want)
			}
		})
	}
}

func TestPGExistingLongTextLengthBounds(t *testing.T) {
	for _, state := range []string{"valid", "nonempty", "missing", "disabled", "weaker", "wrong_column", "byte_length", "or_true"} {
		t.Run(state, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			profile, _ := (&pgPlanWriter{}).InspectMySQL(context.Background())
			col := source.ColumnInfo{Name: "Body", DataType: "varchar", Length: 4000, IsNullable: true}
			table := source.PGTable{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{col}}}
			p := &PGMySQLPreflight{Target: profile, tables: []source.PGTable{table}}
			m := NewMigrator(&pgPlanReader{}, &pgPlanWriter{db: db}, &Job{}, Config{Content: "data_only"})
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
			mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
			mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}).AddRow("InnoDB", "BASE TABLE", "Dynamic"))
			mock.ExpectQuery(`SELECT COLUMN_NAME,COLUMN_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"n", "t", "null", "extra", "co", "def", "gen"}).AddRow("Body", "longtext", "YES", "", "utf8mb4_bin", nil, nil))
			existingRows := sqlmock.NewRows([]string{"n"})
			if state == "nonempty" {
				existingRows.AddRow(1)
			}
			mock.ExpectQuery("SELECT 1 FROM .* LIMIT 1").WillReturnRows(existingRows)
			mock.ExpectQuery(`SELECT INDEX_NAME`).WillReturnRows(sqlmock.NewRows([]string{"n", "c", "u", "p", "t"}))
			checks := sqlmock.NewRows([]string{"n", "expr", "enforced"})
			expr, enforced := "(char_length(`Body`) <= 4000)", "YES"
			switch state {
			case "disabled":
				enforced = "NO"
			case "weaker":
				expr = "char_length(`Body`) <= 4001"
			case "wrong_column":
				expr = "char_length(`Other`) <= 4000"
			case "byte_length":
				expr = "length(`Body`) <= 4000"
			case "or_true":
				expr = "(char_length(`Body`) <= 4000) OR TRUE"
			}
			if state != "missing" {
				checks.AddRow("T_chk_1", expr, enforced)
			}
			mock.ExpectQuery(`SELECT tc.CONSTRAINT_NAME`).WillReturnRows(checks)
			// Unsupported CHECK expressions fail closed before later queries.
			if state == "valid" || state == "nonempty" || state == "missing" {
				mock.ExpectQuery(`SELECT k.CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"n", "c", "rs", "rt", "rc", "d", "u"}))
				mock.ExpectQuery(`SELECT EVENT_OBJECT_TABLE`).WillReturnRows(sqlmock.NewRows([]string{"t", "n"}))
			}
			err := m.inspectPGMySQLTarget(context.Background(), p, db, true)
			p.finish()
			if (err == nil && p.Ready) != (state == "valid") {
				t.Fatalf("err=%v issues=%+v", err, p.Issues)
			}
			if !p.tables[0].Info.Columns[0].PGMySQLLongText {
				t.Fatal("target mapping lost")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPGWideColumnDefaultsAndVersion(t *testing.T) {
	def := "'中文'::varchar"
	c := source.ColumnInfo{Name: "Body", DataType: "varchar", Length: 4000, Default: &def, PGMySQLLongText: true}
	for _, version := range []dialect.MySQLVersion{{Major: 5, Minor: 7, Patch: 44}, {Major: 8, Minor: 0, Patch: 32}} {
		table := source.PGTable{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{c}}}
		stmts, _, err := dialect.NewMySQL().PGTableStatements("dest", table, version, strings.ToLower)
		if (err == nil) != version.Checks() {
			t.Fatal(version, err)
		}
		if err == nil {
			if !strings.Contains(stmts[1].SQL, "`body` LONGTEXT NOT NULL DEFAULT ('中文')") || !strings.Contains(stmts[1].SQL, "CHAR_LENGTH(`body`) <= 4000") {
				t.Fatal(stmts)
			}
		}
	}
}
