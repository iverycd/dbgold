package datamigrate

import (
	"context"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"

	"github.com/DATA-DOG/go-sqlmock"
)

type pgNumericReader struct {
	pgPlanReader
	pages int
}

func (r *pgNumericReader) DBType() string { return "vastbase" }
func (r *pgNumericReader) ReadPage(_ context.Context, _ string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	r.pages++
	if offset >= int64(len(r.rows)) {
		return []string{"id"}, nil, nil, nil
	}
	end := min(offset+limit, int64(len(r.rows)))
	return []string{"id"}, nil, r.rows[offset:end], nil
}

func numericReader(rows [][]any) *pgNumericReader {
	return &pgNumericReader{pgPlanReader: pgPlanReader{
		snapshot: &source.PGSnapshot{Profile: source.PGProfile{Product: "vastbase", Schema: "public"}, Tables: []source.PGTable{{
			Info: source.TableDDLInfo{TableName: "MixedTable", Columns: []source.ColumnInfo{{
				Name: "id", DataType: "numeric", Precision: 255, Scale: 10, IsNullable: true,
			}}},
		}}}, rows: rows,
	}}
}

func TestPGNumericNarrowingPreflight(t *testing.T) {
	maxValue := strings.Repeat("9", 55) + "." + strings.Repeat("9", 10)
	for _, content := range []string{"both", "schema_only"} {
		for _, tc := range []struct {
			name  string
			rows  [][]any
			ready bool
		}{
			{"exact_bounds", [][]any{{maxValue}, {"-" + maxValue}, {"0.0000000001"}, {nil}}, true},
			{"empty", nil, true},
			{"late_positive_overflow", [][]any{{"1"}, {"1e55"}}, false},
			{"late_negative_overflow", [][]any{{"1"}, {"-1e55"}}, false},
			{"fractional_loss", [][]any{{"1.00000000001"}}, false},
			{"non_finite", [][]any{{"NaN"}}, false},
		} {
			t.Run(content+"/"+tc.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				r := numericReader(tc.rows)
				w := &pgPlanWriter{db: db}
				m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: content, PageSize: 1})
				expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
				p := m.PreflightPGMySQL(context.Background())
				if !p.Ready || p.CheckScope != "structure" {
					t.Fatalf("ready=%v issues=%+v", p.Ready, p.Issues)
				}
				if r.snapshot.Tables[0].Info.Columns[0].Precision != 255 {
					t.Fatal("source metadata mutated")
				}
				if len(p.tables) != 1 || p.tables[0].Info.Columns[0].Precision != 65 {
					t.Fatal("missing target precision")
				}
				warning, ddl := false, false
				for _, issue := range p.Issues {
					if issue.Severity == "warning" && strings.Contains(issue.Reason, "numeric(255,10)") && strings.Contains(issue.Suggestion, "55 位整数") {
						warning = true
					}
				}
				for _, obj := range p.Objects {
					if strings.Contains(dialect.JoinSQL(obj.Statements), "`id` DECIMAL(65,10)") {
						ddl = true
					}
				}
				if !warning || !ddl {
					t.Fatalf("warning=%v ddl=%v", warning, ddl)
				}
				if p.RowsChecked != 0 || r.pages != 0 {
					t.Fatalf("structure preflight scanned rows: checked=%d pages=%d", p.RowsChecked, r.pages)
				}
				_, conversionErr := convertPGPage(p.tables[0], []string{"id"}, tc.rows)
				if (conversionErr == nil) != tc.ready {
					t.Fatalf("copy validation: %v", conversionErr)
				}
				if w.mutations != 0 {
					t.Fatal("preflight mutated target")
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPGNumericStartupRechecksAndCopyRejectsOverflow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := numericReader([][]any{{"1234.5678"}})
	w := &pgPlanWriter{db: db}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "both", PageSize: 1})
	expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
	p := m.PreflightPGMySQL(context.Background())
	if !p.Ready {
		t.Fatalf("issues=%+v", p.Issues)
	}
	converted, err := convertPGPage(p.tables[0], []string{"id"}, r.rows)
	if err != nil || converted[0][0] != "1234.5678000000" {
		t.Fatalf("converted=%v err=%v", converted, err)
	}
	r.rows = [][]any{{"1e55"}}
	if _, err := convertPGPage(p.tables[0], []string{"id"}, r.rows); err == nil || !strings.Contains(err.Error(), "MixedTable.id") || !strings.Contains(err.Error(), "DECIMAL(65,10)") {
		t.Fatalf("copy failed to reject overflow with column context: %v", err)
	}
	expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
	expectPGTextRecheck(mock)
	report := m.Run(context.Background())
	if !report.HasFailures() || !report.Preflight.Ready || w.mutations != 1 {
		t.Fatalf("unsafe run: %+v mutations=%d", report, w.mutations)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPGNumericDefaultUsesTargetPrecision(t *testing.T) {
	for _, raw := range []string{"0.1234567890", "1e55"} {
		t.Run(raw, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			r := numericReader(nil)
			r.snapshot.Tables[0].Info.Columns[0].Default = &raw
			m := NewMigrator(r, &pgPlanWriter{db: db}, &Job{LogCh: make(chan string, 128)}, Config{Mode: "all", Content: "schema_only"})
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema.SCHEMATA`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
			mock.ExpectQuery(`SELECT TABLE_SCHEMA,TABLE_NAME,CONSTRAINT_NAME`).WillReturnRows(sqlmock.NewRows([]string{"s", "t", "c", "rs", "rt"}))
			mock.ExpectQuery(`SELECT ENGINE,TABLE_TYPE`).WillReturnRows(sqlmock.NewRows([]string{"e", "t", "r"}))
			mock.ExpectQuery(`SELECT TABLE_NAME,VIEW_DEFINITION`).WillReturnRows(sqlmock.NewRows([]string{"name", "def"}))
			if raw != "1e55" {
				mock.ExpectQuery(`SHOW GRANTS`).WillReturnRows(sqlmock.NewRows([]string{"grant"}).AddRow("GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'"))
			}
			p := m.PreflightPGMySQL(context.Background())
			if p.Ready != (raw != "1e55") {
				t.Fatalf("issues=%+v", p.Issues)
			}
			if p.Ready && !strings.Contains(dialect.JoinSQL(p.Objects[0].Statements), "DEFAULT 0.1234567890") {
				t.Fatal("default lost precision")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
