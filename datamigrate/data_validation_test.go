package datamigrate

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type contentTestStream struct {
	rows   [][]any
	offset int
}

func (s *contentTestStream) Next(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.offset == len(s.rows) {
		return nil, io.EOF
	}
	row := s.rows[s.offset]
	s.offset++
	return row, nil
}
func (*contentTestStream) Close() error { return nil }

type contentTestReader struct {
	*mockReader
	metadata         validation.Table
	describes, scans int
	cleanText        bool
}

func (r *contentTestReader) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	r.describes++
	return r.metadata, nil
}
func (r *contentTestReader) OpenValidationScan(_ context.Context, table string, _ []validation.Column) (validation.RowStream, error) {
	r.scans++
	return &contentTestStream{rows: r.rows[table]}, nil
}
func (r *contentTestReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}
func (r *contentTestReader) ReadPage(_ context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	cols, types := []string{}, []string{}
	for _, c := range r.metadata.Columns {
		cols = append(cols, c.Name)
		types = append(types, c.DatabaseType)
	}
	rows := r.rows[table]
	if offset >= int64(len(rows)) {
		return cols, types, nil, nil
	}
	out := [][]any{}
	for _, row := range rows[offset:min(int64(len(rows)), offset+limit)] {
		copy := slices.Clone(row)
		if r.cleanText {
			for i, v := range copy {
				if s, ok := v.(string); ok {
					copy[i] = strings.ReplaceAll(s, "\x00", "")
				}
			}
		}
		out = append(out, copy)
	}
	return cols, types, out, nil
}

type contentTestWriter struct {
	mockWriter
	metadata         validation.Table
	rows             map[string][][]any
	describes, scans int
	corrupt          bool
	afterCopy        func()
}

func (w *contentTestWriter) DescribeValidationTable(context.Context, string) (validation.Table, error) {
	w.describes++
	return w.metadata, nil
}
func (w *contentTestWriter) OpenValidationScan(_ context.Context, table string, _ []validation.Column) (validation.RowStream, error) {
	w.scans++
	return &contentTestStream{rows: w.rows[table]}, nil
}
func (w *contentTestWriter) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(w.rows[table])), nil
}
func (w *contentTestWriter) CopyData(_ context.Context, table string, _ []string, _ []string, rows [][]any) error {
	if w.copyDataFail {
		return fmt.Errorf("injected copy error")
	}
	for _, row := range rows {
		copy := slices.Clone(row)
		if w.corrupt {
			copy[len(copy)-1] = "wrong"
		}
		w.rows[table] = append(w.rows[table], copy)
	}
	if w.afterCopy != nil {
		w.afterCopy()
	}
	return nil
}

func contentFixture() (*contentTestReader, *contentTestWriter, *Migrator) {
	columns := []validation.Column{{Name: "ID", DatabaseType: "INT", Kind: validation.Number}, {Name: "Text", DatabaseType: "VARCHAR", Kind: validation.Text}}
	r := &contentTestReader{mockReader: &mockReader{tables: []string{"Users"}, ddl: map[string]*source.TableDDLInfo{"Users": {TableName: "Users", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}, {Name: "Text", DataType: "varchar", Length: 100}}}}, pk: map[string][]string{"Users": {"ID"}}, rows: map[string][][]any{"Users": {{int64(1), "original"}}}}, metadata: validation.Table{Columns: columns, PrimaryKey: []string{"ID"}}}
	dstColumns := slices.Clone(columns)
	dstColumns[0].Name = "id"
	dstColumns[1].Name = "text"
	w := &contentTestWriter{metadata: validation.Table{Columns: dstColumns, PrimaryKey: []string{"id"}}, rows: map[string][][]any{}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 512)}, Config{Mode: "all", PageSize: 10, MaxParallel: 1, LowerCaseNames: true, ValidateData: true})
	return r, w, m
}

func TestContentValidationMigrationDefaultDoesNotRead(t *testing.T) {
	r, w, m := contentFixture()
	m.cfg.ValidateData = false
	report := m.Run(context.Background())
	require.Nil(t, report.DataValidation)
	require.Zero(t, r.describes)
	require.Zero(t, r.scans)
	require.Zero(t, w.describes)
	require.Zero(t, w.scans)
}

func TestContentValidationMigrationDetectsSameCountDifference(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			r, w, m := contentFixture()
			w.corrupt = corrupt
			report := m.Run(context.Background())
			require.NotNil(t, report.DataValidation)
			require.Len(t, report.RowCounts, 1)
			require.True(t, report.RowCounts[0].Match)
			if corrupt {
				require.Equal(t, validation.StatusMismatch, report.DataValidation.Status)
				require.EqualValues(t, 1, report.DataValidation.Tables[0].ChangedRows)
				require.True(t, report.HasFailures())
			} else {
				require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
				require.False(t, report.HasFailures())
			}
			require.Greater(t, r.scans, 0)
		})
	}
}

func TestContentValidationReadsOriginalBeforeCharacterCleaner(t *testing.T) {
	r, _, m := contentFixture()
	r.rows["Users"][0][1] = "a\x00b"
	r.cleanText = true
	report := m.Run(context.Background())
	require.True(t, report.RowCounts[0].Match)
	require.Equal(t, validation.StatusMismatch, report.DataValidation.Status)
}

func TestContentValidationCopyFailureCannotPass(t *testing.T) {
	r, w, m := contentFixture()
	w.copyDataFail = true
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, validation.StatusError, report.DataValidation.Status)
	require.Zero(t, r.scans)
	require.Zero(t, w.scans)
}

func TestContentValidationDetectsMetadataChangeAndExtraTargetColumn(t *testing.T) {
	for _, which := range []string{"source", "target"} {
		t.Run(which, func(t *testing.T) {
			r, w, m := contentFixture()
			w.afterCopy = func() {
				if which == "source" {
					r.metadata.Columns = slices.Clone(r.metadata.Columns)
					r.metadata.Columns[1].DatabaseType = "TEXT"
				} else {
					w.metadata.Columns = append(w.metadata.Columns, validation.Column{Name: "unexpected", Kind: validation.Text, DatabaseType: "TEXT"})
				}
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.NotEqual(t, validation.StatusPassed, report.DataValidation.Status)
		})
	}
}

func TestContentValidationExcludedModesDoNotRead(t *testing.T) {
	for _, mode := range []string{"schema_only", "objects", "views"} {
		t.Run(mode, func(t *testing.T) {
			r, w, m := contentFixture()
			switch mode {
			case "schema_only":
				m.cfg.Content = mode
			case "objects":
				m.cfg.Objects = []string{"primary_keys"}
			case "views":
				m.cfg.ViewNames = []string{"v"}
			}
			report := m.Run(context.Background())
			require.Nil(t, report.DataValidation)
			require.Zero(t, r.describes)
			require.Zero(t, r.scans)
			require.Zero(t, w.describes)
			require.Zero(t, w.scans)
		})
	}
}

func TestContentValidationCancelNeverPasses(t *testing.T) {
	_, w, m := contentFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterCopy = cancel
	report := m.Run(ctx)
	require.NotNil(t, report.DataValidation)
	require.Equal(t, validation.StatusCancelled, report.DataValidation.Status)
}

func TestContentValidationDataOnlyIncludesExistingTargetRows(t *testing.T) {
	_, w, m := contentFixture()
	m.cfg.Content = "data_only"
	w.rows["users"] = [][]any{{int64(2), "existing"}}
	report := m.Run(context.Background())
	require.Equal(t, validation.StatusMismatch, report.DataValidation.Status)
	require.EqualValues(t, 1, report.DataValidation.Tables[0].ExtraRows)
}

func TestContentValidationEarlyFailureAndUnsupportedScanner(t *testing.T) {
	r, _, m := contentFixture()
	m.reader = r.mockReader
	report := m.Run(context.Background())
	require.Equal(t, validation.StatusUnsupported, report.DataValidation.Status)
	require.True(t, report.HasFailures())
	_, _, m = contentFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report = m.Run(ctx)
	require.Equal(t, validation.StatusCancelled, report.DataValidation.Status)
}

func TestContentValidationSnapshotUnwrapAndReportFailures(t *testing.T) {
	r, _, _ := contentFixture()
	scanner, ok := validationSource(&sqlServerDamengSnapshot{Reader: r})
	require.True(t, ok)
	require.Equal(t, r, scanner)
	report := newMigrationReport()
	report.DataValidation = &validation.Report{Status: validation.StatusPassed, Tables: []validation.TableResult{{Status: validation.StatusError}}}
	require.True(t, report.HasFailures())
}

func TestContentValidationEmptySelectionIsNotPassed(t *testing.T) {
	r, _, m := contentFixture()
	r.tables = nil
	report := m.Run(context.Background())
	require.Equal(t, validation.StatusIncomplete, report.DataValidation.Status)
	require.True(t, report.HasFailures())
	require.Contains(t, report.DataValidation.Tables[0].Error, "没有可校验的数据表")
}
