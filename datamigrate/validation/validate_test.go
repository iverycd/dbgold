package validation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type memoryScanner struct {
	meta             Table
	rows             [][]any
	describes, opens int
	onDescribe       func(*memoryScanner)
	onNext           func(*memoryScanner, int)
	onOpen           func(*memoryScanner)
	nextError        error
	closeError       error
}

func (s *memoryScanner) DescribeValidationTable(ctx context.Context, _ string) (Table, error) {
	if e := ctx.Err(); e != nil {
		return Table{}, e
	}
	s.describes++
	if s.onDescribe != nil {
		s.onDescribe(s)
	}
	return Table{Columns: slices.Clone(s.meta.Columns), PrimaryKey: slices.Clone(s.meta.PrimaryKey)}, nil
}
func (s *memoryScanner) OpenValidationScan(ctx context.Context, _ string, columns []Column) (RowStream, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s.opens++
	if s.onOpen != nil {
		s.onOpen(s)
	}
	var positions []int
	for _, c := range columns {
		for i, actual := range s.meta.Columns {
			if c.Name == actual.Name {
				positions = append(positions, i)
				break
			}
		}
	}
	return &memoryStream{scanner: s, positions: positions}, nil
}

type memoryStream struct {
	scanner   *memoryScanner
	positions []int
	index     int
}

func (r *memoryStream) Next(ctx context.Context) ([]any, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if r.scanner.onNext != nil {
		r.scanner.onNext(r.scanner, r.index)
	}
	if r.index >= len(r.scanner.rows) {
		if r.scanner.nextError != nil {
			return nil, r.scanner.nextError
		}
		return nil, io.EOF
	}
	row := r.scanner.rows[r.index]
	r.index++
	out := make([]any, len(r.positions))
	for i, p := range r.positions {
		out[i] = row[p]
	}
	return out, nil
}
func (r *memoryStream) Close() error { return r.scanner.closeError }
func keyedFixtures() (*memoryScanner, *memoryScanner, Plan) {
	sourceColumns := []Column{{Name: "ID", Kind: Number, DatabaseType: "bigint"}, {Name: "Body", Kind: Text, DatabaseType: "text"}}
	targetColumns := []Column{{Name: "id", Kind: Number, DatabaseType: "numeric"}, {Name: "body", Kind: Text, DatabaseType: "varchar"}}
	src := &memoryScanner{meta: Table{Columns: sourceColumns, PrimaryKey: []string{"ID"}}}
	dst := &memoryScanner{meta: Table{Columns: []Column{targetColumns[1], targetColumns[0]}, PrimaryKey: []string{"id"}}}
	p := Plan{SourceTable: "Mixed", TargetTable: "mixed", SourceColumns: slices.Clone(sourceColumns), TargetColumns: targetColumns, PrimaryKey: []string{"ID"}}
	return src, dst, p
}
func testOptions(t *testing.T) options { return options{records: 2, fanIn: 2, tempParent: t.TempDir()} }

func TestValidateKeyedDifferencesAndMappedCatalogOrder(t *testing.T) {
	src, dst, p := keyedFixtures()
	src.rows = [][]any{{"1", "same"}, {"2", "old"}, {"3", strings.Repeat("你", 300)}, {"5", "missing"}}
	dst.rows = [][]any{{"extra", "4"}, {"new", "2"}, {"same", "1"}}
	opt := testOptions(t)
	r := validateWithOptions(context.Background(), src, dst, p, opt)
	require.Equal(t, StatusMismatch, r.Status, r.Error)
	require.EqualValues(t, 4, r.SourceRows)
	require.EqualValues(t, 3, r.TargetRows)
	require.EqualValues(t, 2, r.MissingRows)
	require.EqualValues(t, 1, r.ExtraRows)
	require.EqualValues(t, 1, r.ChangedRows)
	require.Len(t, r.Samples, 4)
	var changed, missingLong bool
	for _, s := range r.Samples {
		require.Len(t, s.KeyHash, 64)
		require.Contains(t, s.Key, "ID")
		if s.Type == "changed" {
			changed = true
			require.Equal(t, []string{"Body"}, s.Columns)
			require.Len(t, s.Fields, 1)
			require.Equal(t, "old", s.Fields[0].Source.Text)
			require.Equal(t, "new", s.Fields[0].Target.Text)
		}
		for _, f := range s.Fields {
			if f.Source != nil && f.Source.Truncated {
				missingLong = true
				require.Equal(t, strings.Repeat("你", 256), f.Source.Text)
			}
		}
	}
	require.True(t, changed)
	require.True(t, missingLong)
	require.Equal(t, 2, src.describes)
	require.Equal(t, 2, dst.describes)
	entries, e := os.ReadDir(opt.tempParent)
	require.NoError(t, e)
	require.Empty(t, entries)
}

func TestValidateMultiPassSortIgnoresPhysicalOrder(t *testing.T) {
	src, dst, p := keyedFixtures()
	for i := 0; i < 75; i++ {
		src.rows = append(src.rows, []any{fmt.Sprint(i), fmt.Sprintf("body-%d", i)})
		dst.rows = append(dst.rows, []any{fmt.Sprintf("body-%d", 74-i), fmt.Sprint(74 - i)})
	}
	r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
	require.Equal(t, StatusPassed, r.Status, r.Error)
	require.EqualValues(t, 75, r.SourceRows)
	require.Empty(t, r.Samples)
	require.Equal(t, 1, src.opens)
	require.Equal(t, 1, dst.opens)
}

func TestValidateKeylessPreservesDuplicateMultiplicity(t *testing.T) {
	c := []Column{{Name: "value", Kind: Text, DatabaseType: "text"}}
	src := &memoryScanner{meta: Table{Columns: c}, rows: [][]any{{"A"}, {"A"}, {"B"}, {"B"}}}
	dst := &memoryScanner{meta: Table{Columns: c}, rows: [][]any{{"C"}, {"C"}, {"D"}, {"D"}}}
	r := validateWithOptions(context.Background(), src, dst, Plan{SourceTable: "s", TargetTable: "t"}, testOptions(t))
	require.Equal(t, StatusMismatch, r.Status, r.Error)
	require.EqualValues(t, 4, r.MissingRows)
	require.EqualValues(t, 4, r.ExtraRows)
	require.Zero(t, r.ChangedRows)
	require.Zero(t, r.DuplicateKeys)
	for _, sample := range r.Samples {
		require.Empty(t, sample.KeyHash)
		require.Len(t, sample.RowHash, 64)
	}
	dst.rows = [][]any{{"B"}, {"A"}, {"B"}, {"A"}}
	r = validateWithOptions(context.Background(), src, dst, Plan{SourceTable: "s", TargetTable: "t"}, testOptions(t))
	require.Equal(t, StatusPassed, r.Status, r.Error)
}

func TestValidateDuplicatePrimaryKeysFailEvenWhenRowsMatch(t *testing.T) {
	src, dst, p := keyedFixtures()
	src.rows = [][]any{{"1", "same"}, {"1", "same"}}
	dst.rows = [][]any{{"same", "1"}, {"same", "1"}}
	r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
	require.Equal(t, StatusMismatch, r.Status)
	require.EqualValues(t, 1, r.DuplicateKeys)
	require.Equal(t, "duplicate_key", r.Samples[0].Type)
}

func TestValidateSampleLimitAndNoRowValuesInTemporaryFiles(t *testing.T) {
	src, dst, p := keyedFixtures()
	const secret = "not-for-temporary-storage"
	for i := 0; i < 40; i++ {
		src.rows = append(src.rows, []any{fmt.Sprint(i), secret})
		dst.rows = append(dst.rows, []any{"changed", fmt.Sprint(i)})
	}
	opt := testOptions(t)
	opt.records = 1
	inspected := false
	src.onNext = func(s *memoryScanner, index int) {
		if index != 1 || s.opens != 1 {
			return
		}
		e := filepath.WalkDir(opt.tempParent, func(path string, d os.DirEntry, e error) error {
			require.NoError(t, e)
			info, e := d.Info()
			require.NoError(t, e)
			if d.IsDir() {
				if path != opt.tempParent {
					require.Equal(t, os.FileMode(0700), info.Mode().Perm())
				}
				return nil
			}
			b, e := os.ReadFile(path)
			require.NoError(t, e)
			require.NotContains(t, string(b), secret)
			require.Zero(t, len(b)%recordBytes)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			inspected = true
			return nil
		})
		require.NoError(t, e)
	}
	r := validateWithOptions(context.Background(), src, dst, p, opt)
	require.Equal(t, StatusMismatch, r.Status, r.Error)
	require.EqualValues(t, 40, r.ChangedRows)
	require.Len(t, r.Samples, maxSamples)
	require.True(t, inspected)
}

func TestValidateFrozenMetadataAndCompleteTargetScope(t *testing.T) {
	t.Run("source changed before scan", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		src.meta.Columns[0].Precision = 25
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusUnsupported, r.Status)
		require.Zero(t, src.opens)
	})
	t.Run("source changed during scan", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		src.onDescribe = func(s *memoryScanner) {
			if s.describes == 2 {
				s.meta.PrimaryKey = nil
			}
		}
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusUnsupported, r.Status)
		require.Contains(t, r.Error, "主键")
	})
	t.Run("extra target column", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		dst.meta.Columns = append(dst.meta.Columns, Column{Name: "extra", Kind: Text})
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusUnsupported, r.Status)
		require.Zero(t, dst.opens)
	})
	t.Run("target changed during scan", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		dst.onDescribe = func(s *memoryScanner) {
			if s.describes == 2 {
				s.meta.Columns[0].Length++
			}
		}
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusUnsupported, r.Status)
	})
}

func TestValidateIncompleteAndCancellationNeverPass(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		src.nextError = errors.New("read failed")
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusError, r.Status)
		require.Contains(t, r.Error, "read failed")
	})
	t.Run("close error", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		src.closeError = errors.New("cursor close failed")
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusError, r.Status)
	})
	t.Run("cancel removes temporary files", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		src.rows = [][]any{{"1", "data"}}
		ctx, cancel := context.WithCancel(context.Background())
		src.onNext = func(_ *memoryScanner, index int) {
			if index == 1 {
				cancel()
			}
		}
		opt := testOptions(t)
		r := validateWithOptions(ctx, src, dst, p, opt)
		require.Equal(t, StatusCancelled, r.Status)
		entries, e := os.ReadDir(opt.tempParent)
		require.NoError(t, e)
		require.Empty(t, entries)
	})
	t.Run("sample changed between reads", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		src.rows = [][]any{{"1", "old"}}
		dst.rows = [][]any{{"new", "1"}}
		src.onOpen = func(s *memoryScanner) {
			if s.opens == 2 {
				s.rows = [][]any{{"1", "newer"}}
			}
		}
		r := validateWithOptions(context.Background(), src, dst, p, testOptions(t))
		require.Equal(t, StatusUnsupported, r.Status)
		require.Contains(t, r.Error, "两次读取之间")
	})
	t.Run("temporary path unavailable", func(t *testing.T) {
		src, dst, p := keyedFixtures()
		opt := testOptions(t)
		opt.tempParent = filepath.Join(opt.tempParent, "absent")
		r := validateWithOptions(context.Background(), src, dst, p, opt)
		require.Equal(t, StatusError, r.Status)
	})
}

func TestReportFailuresIncludeIncomplete(t *testing.T) {
	for _, status := range []string{StatusMismatch, StatusError, StatusUnsupported, StatusCancelled, StatusIncomplete} {
		r := Report{Tables: []TableResult{{Status: StatusPassed}, {Status: status}}}
		r.Finalize()
		require.True(t, r.HasFailures())
		require.Equal(t, status, r.Status)
	}
	r := Report{Tables: []TableResult{{Status: StatusPassed}}}
	r.Finalize()
	require.False(t, r.HasFailures())
}
