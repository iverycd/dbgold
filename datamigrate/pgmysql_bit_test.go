package datamigrate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPGBitPreflightAndTargetReadback(t *testing.T) {
	for _, width := range []int64{1, 8, 64} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			bits := strings.Repeat("1", int(width))
			raw := "B'" + bits + "'"
			col := source.ColumnInfo{Name: "id", DataType: "bit", Length: width, Default: &raw, IsNullable: true}
			r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{col}}}}}, rows: [][]any{{[]byte(bits)}, {nil}}}
			w := &pgPlanWriter{db: db}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "both"})
			expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			p := m.PreflightPGMySQL(context.Background())
			if !p.Ready || p.RowsChecked != 0 || w.mutations != 0 {
				t.Fatalf("unsafe plan: %+v", p)
			}
			if !strings.Contains(dialect.JoinSQL(p.Objects[0].Statements), fmt.Sprintf("BIT(%d) DEFAULT b'%s'", width, bits)) {
				t.Fatalf("wrong DDL: %+v", p.Objects)
			}
			var want uint64 = ^uint64(0)
			if width < 64 {
				want = (uint64(1) << width) - 1
			}
			query := fmt.Sprintf("SELECT LPAD(BIN(CAST(`id` AS UNSIGNED)), %d, '0') FROM `target`.`T` ORDER BY `id` LIMIT 2000 OFFSET 0", width)
			mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow([]byte(bits)).AddRow(nil))
			err = m.eachPGPage(context.Background(), p.tables[0], db, func(_ []string, rows [][]any) error {
				if rows[0][0] != want || rows[1][0] != nil {
					t.Fatalf("incorrect roundtrip: %v", rows)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPGBitValuesAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		bits  string
		width int64
		want  uint64
	}{
		{"0", 1, 0}, {"1", 1, 1}, {"00110001", 8, 49}, {strings.Repeat("1", 64), 64, ^uint64(0)},
	} {
		c := source.ColumnInfo{DataType: "bit", Length: tc.width}
		for _, input := range []any{tc.bits, []byte(tc.bits)} {
			got, err := valueconv.PGMySQLValue(input, c)
			if err != nil || got != tc.want {
				t.Fatalf("bits=%s got=%v err=%v", tc.bits, got, err)
			}
		}
	}
	for _, input := range []any{"", "2", "00", "true", []byte{0}, []byte{1}} {
		if _, err := valueconv.PGMySQLValue(input, source.ColumnInfo{DataType: "bit", Length: 1}); err == nil {
			t.Fatalf("accepted invalid bits: %v", input)
		}
	}
	for _, c := range []source.ColumnInfo{{DataType: "bit", Length: 0}, {DataType: "bit", Length: 65}, {DataType: "varbit", Length: 8}} {
		if _, err := typemap.PGToMySQLChecked(c); err == nil {
			t.Fatalf("unsupported type accepted: %+v", c)
		}
	}
	for _, tc := range []struct {
		raw   string
		width int64
		want  string
	}{
		{"'0'::bit", 1, " DEFAULT b'0'"}, {"(B'1')", 1, " DEFAULT b'1'"}, {"('00110001'::bit(8))", 8, " DEFAULT b'00110001'"}, {"NULL::bit", 1, " DEFAULT NULL"},
		{"B'2'", 1, ""}, {"B'01'", 1, ""}, {"'00000001'::bit", 8, ""}, {"'1'::bit(8)", 8, ""}, {"1::bit", 1, ""}, {"B'1'; DROP TABLE t", 1, ""}, {"'11111111'::bit(8)::bit(1)", 1, ""},
	} {
		c := source.ColumnInfo{DataType: "bit", Length: tc.width, Default: &tc.raw, IsNullable: true}
		got, err := dialect.PGColumnDefault(c, fmt.Sprintf("BIT(%d)", tc.width), dialect.MySQLVersion{Major: 8})
		if tc.want == "" {
			if err == nil {
				t.Fatalf("accepted unsafe default %s: %s", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("default %s: got=%s err=%v", tc.raw, got, err)
		}
	}
}

func TestPGUnsupportedColumnReportedOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := &pgPlanReader{snapshot: &source.PGSnapshot{Profile: source.PGProfile{Schema: "public"}, Tables: []source.PGTable{{Info: source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "disabled", DataType: "bit", Length: 65}}}}}}}
	m := NewMigrator(r, &pgPlanWriter{db: db}, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "both"})
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
	mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}))
	mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "def"}))
	p := m.PreflightPGMySQL(context.Background())
	n := 0
	for _, issue := range p.Issues {
		if issue.Severity == "error" {
			n++
			if !strings.Contains(issue.Reason, "disabled") {
				t.Fatalf("missing column: %+v", issue)
			}
		}
	}
	if p.Ready || n != 1 {
		t.Fatalf("duplicate/missing issues: %+v", p.Issues)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
