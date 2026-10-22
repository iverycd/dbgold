package datamigrate

import (
	"context"
	"fmt"
	"io"
	"slices"
	"testing"

	"dbgold/datamigrate/validation"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// These scanners wrap the existing executor fixtures without changing their
// optional migration capabilities. The rows stand in for fresh database reads.
type pathValidationScanner struct {
	metadata           map[string]validation.Table
	rows               map[string][][]any
	described, scanned []string
	selected           map[string][]string
}

func pathScanner(tables []string, column string, rows [][]any) *pathValidationScanner {
	s := &pathValidationScanner{metadata: map[string]validation.Table{}, rows: map[string][][]any{}, selected: map[string][]string{}}
	for _, table := range tables {
		s.metadata[table] = validation.Table{Columns: []validation.Column{{Name: column, DatabaseType: "NUMERIC", Kind: validation.Number}}, PrimaryKey: []string{column}}
		s.rows[table] = rows
	}
	return s
}

func (s *pathValidationScanner) DescribeValidationTable(_ context.Context, table string) (validation.Table, error) {
	s.described = append(s.described, table)
	metadata, ok := s.metadata[table]
	if !ok {
		return validation.Table{}, fmt.Errorf("unexpected validation table %s", table)
	}
	return metadata, nil
}

func (s *pathValidationScanner) OpenValidationScan(_ context.Context, table string, columns []validation.Column) (validation.RowStream, error) {
	s.scanned = append(s.scanned, table)
	for _, column := range columns {
		s.selected[table] = append(s.selected[table], column.Name)
	}
	return &pathValidationStream{rows: s.rows[table]}, nil
}

type pathValidationStream struct {
	rows  [][]any
	index int
}

func (s *pathValidationStream) Next(ctx context.Context) ([]any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.index == len(s.rows) {
		return nil, io.EOF
	}
	row := s.rows[s.index]
	s.index++
	return row, nil
}
func (*pathValidationStream) Close() error { return nil }

type validationOraclePathReader struct {
	*oracleFixtureReader
	*pathValidationScanner
}
type validationOraclePathWriter struct {
	*oracleRecordingWriter
	*pathValidationScanner
}
type validationSQLServerPathReader struct {
	*sqlServerFixtureReader
	*pathValidationScanner
}
type validationSQLServerPathWriter struct {
	*sqlServerRecordingWriter
	*pathValidationScanner
}
type validationPGMySQLPathReader struct {
	*pgStructureReader
	*pathValidationScanner
}
type validationPGMySQLPathWriter struct {
	*pgStructureWriter
	*pathValidationScanner
}

func (r *validationPGMySQLPathReader) ReadPage(ctx context.Context, table string, pk []string, offset, limit int64) ([]string, []string, [][]any, error) {
	_, types, rows, err := r.pgStructureReader.ReadPage(ctx, table, pk, offset, limit)
	return []string{"MixedID"}, types, rows, err
}

func checkContentPathResult(t *testing.T, report MigrationReport, scenario string, src, dst *pathValidationScanner, sourceTable, targetTable, sourceColumn, targetColumn string) {
	t.Helper()
	if scenario == "disabled" {
		require.Nil(t, report.DataValidation)
		require.Empty(t, src.described)
		require.Empty(t, dst.described)
		require.Empty(t, src.scanned)
		require.Empty(t, dst.scanned)
		return
	}
	require.NotNil(t, report.DataValidation)
	if scenario == "early_failure" {
		require.True(t, report.HasFailures())
		require.NotEqual(t, validation.StatusPassed, report.DataValidation.Status)
		require.False(t, slices.Contains(src.scanned, sourceTable), "failed table must not be declared copied or scanned")
		return
	}
	require.Contains(t, src.scanned, sourceTable)
	require.Contains(t, dst.scanned, targetTable)
	require.Contains(t, src.selected[sourceTable], sourceColumn)
	require.Contains(t, dst.selected[targetTable], targetColumn)
	if scenario == "mismatch" {
		require.Equal(t, validation.StatusMismatch, report.DataValidation.Status)
		require.True(t, report.HasFailures())
	} else {
		require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
		require.False(t, report.HasFailures(), "%+v", report)
	}
}

func TestContentValidationOracleExecutor(t *testing.T) {
	for _, scenario := range []string{"passed", "mismatch", "early_failure", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			r, w, m := oracleMigrationFixture()
			src := pathScanner([]string{"Orders", "Items"}, "id", [][]any{{int64(1)}})
			dst := pathScanner([]string{"ORDERS", "ITEMS"}, "ID", [][]any{{int64(1)}})
			if scenario == "mismatch" {
				dst.rows["ORDERS"] = [][]any{{int64(9)}}
			}
			if scenario == "early_failure" {
				w.failTable = "ORDERS"
			}
			m.reader, m.writer = &validationOraclePathReader{r, src}, &validationOraclePathWriter{w, dst}
			m.cfg.ValidateData = scenario != "disabled"
			report := m.Run(context.Background())
			checkContentPathResult(t, report, scenario, src, dst, "Orders", "ORDERS", "id", "ID")
		})
	}
}

func TestContentValidationSQLServerExecutor(t *testing.T) {
	for _, scenario := range []string{"passed", "mismatch", "early_failure", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			r, w, m := sqlServerFixture()
			src := pathScanner([]string{"Orders", "Items"}, "id", [][]any{{int64(1)}})
			dst := pathScanner([]string{"orders", "items"}, "id", [][]any{{int64(1)}})
			if scenario == "mismatch" {
				dst.rows["orders"] = [][]any{{int64(9)}}
			}
			if scenario == "early_failure" {
				w.copyErr = "orders"
			}
			m.reader, m.writer = &validationSQLServerPathReader{r, src}, &validationSQLServerPathWriter{w, dst}
			m.cfg.ValidateData, m.cfg.LowerCaseNames = scenario != "disabled", true
			report := m.Run(context.Background())
			checkContentPathResult(t, report, scenario, src, dst, "Orders", "orders", "id", "id")
		})
	}
}

func TestContentValidationPGMySQLExecutor(t *testing.T) {
	for _, scenario := range []string{"passed", "mismatch", "early_failure", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &pgStructureWriter{pgPlanWriter: pgPlanWriter{db: db}, count: -1}
			r := &pgStructureReader{pgNumericReader: numericReader([][]any{{"1"}, {"2"}}), w: w, count: -1}
			r.snapshot.Tables[0].Info.Columns[0].Name = "MixedID"
			src := pathScanner([]string{"MixedTable"}, "MixedID", [][]any{{"1"}, {"2"}})
			dst := pathScanner([]string{"mixedtable"}, "mixedid", [][]any{{"1"}, {"2"}})
			if scenario == "mismatch" {
				dst.rows["mixedtable"] = [][]any{{"1"}, {"3"}}
			}
			if scenario == "early_failure" {
				w.failDDL = "CREATE TABLE"
			}
			m := NewMigrator(&validationPGMySQLPathReader{r, src}, &validationPGMySQLPathWriter{w, dst}, &Job{LogCh: make(chan string, 512)}, Config{Mode: "all", Content: "both", PageSize: 1, LowerCaseNames: true, ValidateData: scenario != "disabled"})
			expectPGEmptyTarget(mock, "GRANT ALL PRIVILEGES ON *.* TO 'test'@'localhost'")
			expectPGTextRecheck(mock)
			report := m.Run(context.Background())
			checkContentPathResult(t, report, scenario, src, dst, "MixedTable", "mixedtable", "MixedID", "mixedid")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
