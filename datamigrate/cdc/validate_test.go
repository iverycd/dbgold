package cdc

import (
	"context"
	"io"
	"testing"

	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type cutoverTestScanner struct {
	metadata validation.Table
	rows     [][]any
}

func (s cutoverTestScanner) DescribeValidationTable(_ context.Context, _ string) (validation.Table, error) {
	return s.metadata, nil
}

func (s cutoverTestScanner) OpenValidationScan(_ context.Context, _ string, columns []validation.Column) (validation.RowStream, error) {
	indexes := make(map[string]int, len(s.metadata.Columns))
	for i, col := range s.metadata.Columns {
		indexes[col.Name] = i
	}
	rows := make([][]any, len(s.rows))
	for i, row := range s.rows {
		for _, col := range columns {
			rows[i] = append(rows[i], row[indexes[col.Name]])
		}
	}
	return &cutoverTestStream{rows: rows}, nil
}

type cutoverTestStream struct{ rows [][]any }

func (s *cutoverTestStream) Next(_ context.Context) ([]any, error) {
	if len(s.rows) == 0 {
		return nil, io.EOF
	}
	row := s.rows[0]
	s.rows = s.rows[1:]
	return row, nil
}

func (s *cutoverTestStream) Close() error { return nil }

func TestCutoverContentUsesFrozenMappingAndFindsSameCountMismatch(t *testing.T) {
	ctx := context.Background()
	cfg := Config{TargetDBType: "postgres", LowerCaseNames: true}
	counts := []CutoverTableValidation{{CountValidation: CountValidation{Table: "Orders", Source: 1, Target: 1, Match: true}}}
	snapshot := []TableInfo{{Name: "Orders", Columns: []string{"ID", "Status"}, PrimaryKey: []int{0}}}
	src := cutoverTestScanner{metadata: validation.Table{Columns: []validation.Column{
		{Name: "ID", Kind: validation.Number}, {Name: "Status", Kind: validation.Text},
	}, PrimaryKey: []string{"ID"}}, rows: [][]any{{1, "paid"}}}
	dst := cutoverTestScanner{metadata: validation.Table{Columns: []validation.Column{
		{Name: "status", Kind: validation.Text}, {Name: "id", Kind: validation.Number},
	}, PrimaryKey: []string{"id"}}, rows: [][]any{{"open", 1}}}

	results, matched, countsChanged := validateCutoverContents(ctx, cfg, counts, snapshot, src, dst)
	require.False(t, matched)
	require.False(t, countsChanged)
	require.True(t, results[0].Match)
	require.Equal(t, "orders", results[0].Content.TargetTable)
	require.Equal(t, validation.StatusMismatch, results[0].Content.Status)
	require.EqualValues(t, 1, results[0].Content.ChangedRows)
	require.Len(t, results[0].Content.Samples, 1)
	require.Equal(t, []string{"Status"}, results[0].Content.Samples[0].Columns)
	require.Empty(t, results[0].Content.Samples[0].Key)
	require.Empty(t, results[0].Content.Samples[0].Fields)

	dst.rows = [][]any{{"paid", 1}}
	results, matched, countsChanged = validateCutoverContents(ctx, cfg, counts, snapshot, src, dst)
	require.True(t, matched)
	require.False(t, countsChanged)
	require.Equal(t, validation.StatusPassed, results[0].Content.Status)
}

func TestCutoverContentChecksKeylessMultiplicityAndCountDrift(t *testing.T) {
	ctx := context.Background()
	counts := []CutoverTableValidation{{CountValidation: CountValidation{Table: "bag", Source: 2, Target: 2, Match: true}}}
	snapshot := []TableInfo{{Name: "bag", Columns: []string{"value"}}}
	meta := validation.Table{Columns: []validation.Column{{Name: "value", Kind: validation.Text}}}
	src := cutoverTestScanner{metadata: meta, rows: [][]any{{"a"}, {"a"}}}
	dst := cutoverTestScanner{metadata: meta, rows: [][]any{{"a"}, {"b"}}}

	results, matched, countsChanged := validateCutoverContents(ctx, Config{}, counts, snapshot, src, dst)
	require.False(t, matched)
	require.False(t, countsChanged)
	require.Equal(t, validation.StatusMismatch, results[0].Content.Status)
	require.EqualValues(t, 1, results[0].Content.MissingRows)
	require.EqualValues(t, 1, results[0].Content.ExtraRows)

	dst.rows = dst.rows[:1]
	results, matched, countsChanged = validateCutoverContents(ctx, Config{}, counts, snapshot, src, dst)
	require.False(t, matched)
	require.True(t, countsChanged)
	require.Equal(t, validation.StatusMismatch, results[0].Content.Status)

	dst.metadata.Columns = []validation.Column{{Name: "value", Kind: validation.Unsupported}}
	results, matched, countsChanged = validateCutoverContents(ctx, Config{}, counts, snapshot, src, dst)
	require.False(t, matched)
	require.False(t, countsChanged)
	require.Equal(t, validation.StatusUnsupported, results[0].Content.Status)
}
