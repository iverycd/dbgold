package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestPGTextWeightsAndRoundTrip(t *testing.T) {
	for _, lossy := range []bool{false, true} {
		db, mock, _ := sqlmock.New()
		table := source.PGTable{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "K", DataType: "varchar", Length: 20}}}}
		rules := map[string]target.MySQLTextRules{"K": {"latin1", "latin1_swedish_ci", 1, true}}
		back := "é"
		if lossy {
			back = "?"
		}
		mock.ExpectQuery(`SELECT 0,CAST\(CONVERT.*TRIM\(TRAILING WEIGHT_STRING.*FROM WEIGHT_STRING\(v\)\)`).WithArgs([]byte("é")).WillReturnRows(sqlmock.NewRows([]string{"id", "back", "size", "weight"}).AddRow(0, []byte(back), 1, []byte{0, 69}))
		weights, sizes, err := inspectPGTextPage(context.Background(), db, table, rules, [][]any{{"é"}}, map[string]bool{"K": true}, 4096)
		if lossy {
			if err == nil || !strings.Contains(err.Error(), "无法无损表示") {
				t.Fatal(err)
			}
		} else if err != nil || sizes[0][0] != 1 || string(weights[0][0].([]byte)) != "\x00E" {
			t.Fatal(weights, sizes, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestPGTextRulesRecheckedBeforeMutation(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "id", DataType: "int4"}}}}}}}
	w := &pgPlanWriter{db: db}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "schema_only"})
	expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
	mock.ExpectQuery(`SELECT DEFAULT_CHARACTER_SET_NAME,DEFAULT_COLLATION_NAME`).WillReturnRows(sqlmock.NewRows([]string{"cs", "co"}).AddRow("utf8mb4", "utf8mb4_general_ci"))
	mock.ExpectQuery(`SELECT c.CHARACTER_SET_NAME,s.MAXLEN`).WithArgs("utf8mb4_general_ci").WillReturnRows(sqlmock.NewRows([]string{"cs", "max"}).AddRow("utf8mb4", 4))
	mock.ExpectQuery(`SELECT \(CONVERT`).WillReturnRows(sqlmock.NewRows([]string{"pad"}).AddRow(true))
	report := m.Run(context.Background())
	if report.Preflight.Ready || !report.HasFailures() || w.mutations != 0 {
		t.Fatalf("unsafe run: %+v writes=%d", report, w.mutations)
	}
	if !strings.Contains(report.Preflight.Issues[len(report.Preflight.Issues)-1].Reason, "预检后变化") {
		t.Fatal(report.Preflight.Issues)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPGCharacterCapacityUsesActualCharset(t *testing.T) {
	for _, bytes := range []int{1, 4} {
		rules := target.MySQLTextRules{Charset: "latin1", Collation: "latin1_swedish_ci", MaxBytes: bytes, PadSpace: true}
		if bytes == 4 {
			rules.Charset = "utf8mb4"
			rules.Collation = "utf8mb4_bin"
		}
		p := &PGMySQLPreflight{Target: target.MySQLProfile{PageSize: 16384, LargePrefix: true}, tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "value", DataType: "varchar", Length: 20000}}}}}, textRules: map[string]map[string]target.MySQLTextRules{"T": {"value": rules}}}
		m := &Migrator{}
		m.checkPGMySQLTextStructure(p)
		p.finish()
		if p.Ready != (bytes == 1) {
			t.Fatalf("bytes=%d: %+v", bytes, p.Issues)
		}
		p.tables[0].Info.Columns[0].Length = 1000
		idx := source.IndexInfo{Columns: []string{"value"}}
		err := checkPGIndex(p.tables[0], idx, p.Target, p.textRules["T"])
		if (err == nil) != (bytes == 1) {
			t.Fatal(bytes, err)
		}
	}
}

func TestPGForeignKeyCharacterRulesMustMatch(t *testing.T) {
	a := target.MySQLTextRules{"utf8mb4", "utf8mb4_bin", 4, true}
	b := target.MySQLTextRules{"utf8mb4", "utf8mb4_general_ci", 4, true}
	p := &PGMySQLPreflight{textRules: map[string]map[string]target.MySQLTextRules{"child": {"k": a}, "parent": {"k": b}}, validationFKs: []source.FKInfo{{TableName: "child", ConstraintName: "fk", Columns: []string{"k"}, RefTable: "parent", RefColumns: []string{"k"}}}}
	(&Migrator{}).checkPGMySQLTextStructure(p)
	p.finish()
	if p.Ready || len(p.Issues) != 1 || p.Issues[0].Object.Kind != "foreign_key" {
		t.Fatal(p)
	}
}

func TestPGTextDefaultsAreCheckedWithoutData(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	def := "'中文'::varchar"
	p := &PGMySQLPreflight{tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "k", DataType: "varchar", Length: 20, Default: &def}}}}}, textRules: map[string]map[string]target.MySQLTextRules{"T": {"k": {Charset: "latin1", Collation: "latin1_swedish_ci", MaxBytes: 1, PadSpace: true}}}}
	mock.ExpectQuery(`SELECT 0,CAST`).WithArgs([]byte("中文")).WillReturnRows(sqlmock.NewRows([]string{"id", "back", "size", "weight"}).AddRow(0, []byte("??"), 2, nil))
	if err := validatePGTextDefaults(context.Background(), p, db); err == nil || !strings.Contains(err.Error(), "默认值") {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPGRowWidthDiagnostics(t *testing.T) {
	var cols []source.ColumnInfo
	rules := map[string]target.MySQLTextRules{}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("long_column_%d", i)
		cols = append(cols, source.ColumnInfo{Name: name, DataType: "varchar", Length: 2000})
		rules[name] = target.MySQLTextRules{Charset: "utf8mb4", Collation: "utf8mb4_bin", MaxBytes: 4}
	}
	p := &PGMySQLPreflight{Target: target.MySQLProfile{PageSize: 16384}, tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "wide", Columns: cols}}}, textRules: map[string]map[string]target.MySQLTextRules{"wide": rules}}
	(&Migrator{}).checkPGMySQLTextStructure(p)
	p.finish()
	if p.Ready || len(p.Issues) != 1 {
		t.Fatalf("wide table was not blocked: %+v", p.Issues)
	}
	for _, part := range []string{"80022 字节", "65535", "10 列", "long_column_0 ≈ 8002 字节"} {
		if !strings.Contains(p.Issues[0].Reason, part) {
			t.Fatalf("missing %s: %+v", part, p.Issues)
		}
	}
	if !strings.Contains(p.Issues[0].Suggestion, "非索引") {
		t.Fatal("missing index caveat")
	}
	cols = make([]source.ColumnInfo, 1018)
	for i := range cols {
		cols[i] = source.ColumnInfo{Name: fmt.Sprint(i), DataType: "int4"}
	}
	p = &PGMySQLPreflight{Target: target.MySQLProfile{PageSize: 16384}, tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "many", Columns: cols}}}}
	(&Migrator{}).checkPGMySQLTextStructure(p)
	found := false
	for _, issue := range p.Issues {
		if strings.Contains(issue.Reason, "列数 1018") && strings.Contains(issue.Reason, "1017 列") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing column-count diagnostic: %+v", p.Issues)
	}
}
