package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
)

func expectPGTextRecheck(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT DEFAULT_CHARACTER_SET_NAME,DEFAULT_COLLATION_NAME`).WillReturnRows(sqlmock.NewRows([]string{"cs", "co"}).AddRow("utf8mb4", "utf8mb4_bin"))
	mock.ExpectQuery(`SELECT c.CHARACTER_SET_NAME,s.MAXLEN`).WillReturnRows(sqlmock.NewRows([]string{"cs", "max"}).AddRow("utf8mb4", 4))
	mock.ExpectQuery(`SELECT \(CONVERT`).WillReturnRows(sqlmock.NewRows([]string{"pad"}).AddRow(true))
}

type pgStructureWriter struct {
	pgPlanWriter
	committed  int64
	count      int64
	countErr   error
	failDDL    string
	statements []string
}

func (w *pgStructureWriter) CreateTable(_ context.Context, stmts []dialect.Statement) error {
	w.mutations++
	for _, s := range stmts {
		w.statements = append(w.statements, s.SQL)
		if w.failDDL != "" && strings.Contains(s.SQL, w.failDDL) {
			return errors.New("constraint rejected rows")
		}
	}
	return nil
}
func (w *pgStructureWriter) CopyData(_ context.Context, _ string, _, _ []string, rows [][]any) error {
	w.mutations++
	w.committed += int64(len(rows))
	return nil
}
func (w *pgStructureWriter) CountRows(context.Context, string) (int64, error) {
	if w.count >= 0 {
		return w.count, w.countErr
	}
	return w.committed, w.countErr
}

type pgStructureReader struct {
	*pgNumericReader
	w        *pgStructureWriter
	count    int64
	countErr error
}

func (r *pgStructureReader) ReadPage(ctx context.Context, table string, pk []string, offset, limit int64) ([]string, []string, [][]any, error) {
	if r.w.mutations == 0 {
		return nil, nil, nil, errors.New("ReadPage before execution is forbidden")
	}
	return r.pgNumericReader.ReadPage(ctx, table, pk, offset, limit)
}
func (r *pgStructureReader) CountRows(context.Context, string) (int64, error) {
	if r.w.mutations == 0 {
		return 0, errors.New("COUNT before execution is forbidden")
	}
	if r.count >= 0 {
		return r.count, r.countErr
	}
	return int64(len(r.rows)), r.countErr
}

func TestPGStructurePreflightAndRuntimeValidation(t *testing.T) {
	for _, scenario := range []string{"success", "overflow", "negative_auto", "count_mismatch", "both_counts_changed", "count_error", "target_count_error", "index_failure", "schema_only", "seed", "seed_exhausted"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			w := &pgStructureWriter{pgPlanWriter: pgPlanWriter{db: db}, count: -1}
			r := &pgStructureReader{pgNumericReader: numericReader([][]any{{"1"}, {"2"}}), w: w, count: -1}
			cfg := Config{Mode: "all", Content: "both", PageSize: 1}
			switch scenario {
			case "overflow":
				r.rows[1][0] = "1e55"
			case "count_mismatch":
				w.count = 1
			case "both_counts_changed":
				w.count, r.count = 3, 3
			case "count_error":
				r.countErr = errors.New("count unavailable")
			case "target_count_error":
				w.countErr = errors.New("target count unavailable")
			case "schema_only":
				cfg.Content = "schema_only"
			case "index_failure":
				r.snapshot.Tables[0].Indexes = []source.IndexInfo{{TableName: "MixedTable", IndexName: "ux", Columns: []string{"id"}, IsUnique: true, Method: "btree"}}
				w.failDDL = "ADD UNIQUE"
			}
			if scenario == "seed" || scenario == "seed_exhausted" || scenario == "negative_auto" {
				table := &r.snapshot.Tables[0]
				table.Info.Columns[0] = source.ColumnInfo{Name: "id", DataType: "int4", Extra: "auto_increment"}
				table.Sequences = []source.PGSequence{{SequenceInfo: source.SequenceInfo{TableName: "MixedTable", ColumnName: "id", StartValue: 2}, Name: "s", Increment: 1, Min: 1, Max: 2147483647}}
				r.rows = [][]any{{int64(9)}, {int64(10)}}
				if scenario == "seed_exhausted" {
					r.rows[1][0] = int64(2147483647)
				}
				if scenario == "negative_auto" {
					r.rows[1][0] = int64(-1)
				}
			}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 128)}, cfg)
			expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			preview := m.PreflightPGMySQL(context.Background())
			if !preview.Ready || preview.RowsChecked != 0 || preview.CheckScope != "structure" || w.mutations != 0 || r.pages != 0 {
				t.Fatalf("preview=%+v pages=%d", preview, r.pages)
			}
			expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			expectPGTextRecheck(mock)
			report := m.Run(context.Background())
			wantSuccess := scenario == "success" || scenario == "schema_only" || scenario == "seed"
			if report.HasFailures() == wantSuccess || !report.Preflight.Ready {
				t.Fatalf("report=%+v", report)
			}
			if scenario == "overflow" || scenario == "negative_auto" {
				if w.committed != 1 || len(report.Data.Items) != 1 {
					t.Fatal(w.committed, report.Data)
				}
				for _, part := range []string{"MixedTable.id", "第 2 批", "已提交 1 行"} {
					if !strings.Contains(report.Data.Items[0].Error, part) {
						t.Fatal(report.Data.Items)
					}
				}
			}
			if scenario == "schema_only" && (r.pages != 0 || w.committed != 0) {
				t.Fatal("schema-only read data")
			}
			if scenario == "seed" && !strings.Contains(strings.Join(w.statements, "\n"), "AUTO_INCREMENT = 11") {
				t.Fatal(w.statements)
			}
			if scenario == "seed_exhausted" && report.Sequences.Failed != 1 {
				t.Fatal(report.Sequences)
			}
			if scenario == "index_failure" && (report.Indexes.Failed != 1 || !strings.Contains(report.Indexes.Items[0].Error, "已提交 2 行")) {
				t.Fatal(report.Indexes)
			}
			if strings.Contains(scenario, "count") && (report.RowCounts[0].Match || report.Data.Failed != 1 || len(report.Data.Items) != 1) {
				t.Fatal(report)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPGSequenceBounds(t *testing.T) {
	for _, tc := range []struct {
		typ   string
		limit int64
	}{{"int2", 32767}, {"int4", 2147483647}, {"int8", 9223372036854775807}} {
		c := source.ColumnInfo{Name: "id", DataType: tc.typ}
		seq := source.SequenceInfo{TableName: "T", StartValue: 100}
		for _, n := range []int64{0, 50, 101, tc.limit - 1} {
			got, err := pgSequenceStart(c, seq, n)
			if err != nil || got.StartValue != max(100, n+1) {
				t.Fatal(tc, n, got, err)
			}
		}
		if _, err := pgSequenceStart(c, seq, tc.limit); err == nil {
			t.Fatal(fmt.Sprint(tc), "exhausted seed accepted")
		}
	}
}
