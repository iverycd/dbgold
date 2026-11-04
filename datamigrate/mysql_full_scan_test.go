package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type fullScanTestReader struct {
	*mockReader
	mu                                    sync.Mutex
	counts, pages, plans, streams, ranges int
	withoutRange                          bool
	failRead                              string
}

func (r *fullScanTestReader) CountRows(_ context.Context, table string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts++
	return int64(len(r.rows[table])), nil
}

func (r *fullScanTestReader) ReadPage(ctx context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	r.mu.Lock()
	r.pages++
	r.mu.Unlock()
	if table == r.failRead && offset == 2 {
		return nil, nil, nil, errors.New("invalid row on read")
	}
	rows := r.rows[table]
	return []string{"Id"}, []string{"INT"}, rows[offset:min(offset+limit, int64(len(rows)))], ctx.Err()
}

func (r *fullScanTestReader) MySQLScanRanges(_ context.Context, table string, _ int) ([]source.MySQLScanRange, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans++
	if r.withoutRange || len(r.rows[table]) == 0 {
		return nil, nil
	}
	return []source.MySQLScanRange{{Key: "Id", Lower: "1", Upper: "2"}, {Key: "Id", Lower: "3", Upper: "6"}}, nil
}

func (r *fullScanTestReader) StreamMySQLTable(ctx context.Context, table string, span *source.MySQLScanRange, size int, consume func([]string, []string, [][]interface{}) error) error {
	r.mu.Lock()
	if span == nil {
		r.streams++
	} else {
		r.ranges++
	}
	r.mu.Unlock()
	rows := r.rows[table]
	if span != nil {
		from, _ := strconv.Atoi(span.Lower)
		to, _ := strconv.Atoi(span.Upper)
		rows = rows[from-1 : min(to, len(rows))]
	}
	for len(rows) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if table == r.failRead && rows[0][0].(int) == 3 {
			return errors.New("invalid row on read")
		}
		n := min(size, len(rows))
		if err := consume([]string{"Id"}, []string{"INT"}, rows[:n]); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

type fullScanTestWriter struct {
	*mockWriter
	mu           sync.Mutex
	data         map[string][]int
	active, peak int
	failOnTable  string
	started      chan struct{}
	proceed      chan struct{}
}

func (w *fullScanTestWriter) CopyData(ctx context.Context, table string, columns, types []string, rows [][]interface{}) error {
	if len(columns) != 1 || columns[0] != "id" || types[0] != "INT" {
		return fmt.Errorf("unexpected target metadata %v %v", columns, types)
	}
	w.mu.Lock()
	w.active++
	w.peak = max(w.peak, w.active)
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.active--; w.mu.Unlock() }()
	if w.started != nil {
		w.started <- struct{}{}
		select {
		case <-w.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if table == w.failOnTable && rows[0][0].(int) == 3 {
		return errors.New("invalid row")
	}
	for _, row := range rows {
		w.data[table] = append(w.data[table], row[0].(int))
	}
	return nil
}

func (w *fullScanTestWriter) CountRows(_ context.Context, table string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.data[table])), nil
}

func newFullScanTest(strategy string, tables ...string) (*Migrator, *fullScanTestReader, *fullScanTestWriter) {
	r := &fullScanTestReader{mockReader: &mockReader{tables: tables, rows: map[string][][]interface{}{}}}
	for _, table := range tables {
		r.rows[table] = [][]interface{}{{1}, {2}, {3}, {4}, {5}, {6}}
	}
	w := &fullScanTestWriter{mockWriter: &mockWriter{}, data: map[string][]int{}}
	m, _ := newTestMigrator(r, w)
	m.cfg.MySQLReadStrategy, m.cfg.Content, m.cfg.LowerCaseNames = strategy, "data_only", true
	m.cfg.PageSize, m.cfg.IntraTableParallel, m.cfg.MaxParallel = 2, 2, 1
	return m, r, w
}

func TestMySQLFullScanOrdinaryTables(t *testing.T) {
	for _, tc := range []struct {
		strategy                              string
		fallback                              bool
		counts, pages, plans, streams, ranges int
	}{
		{"", false, 4, 3, 0, 0, 0}, {"offset", false, 4, 3, 0, 0, 0},
		{"stream", false, 2, 0, 0, 2, 0}, {"range", false, 2, 0, 2, 1, 2},
		{"range", true, 2, 0, 2, 2, 0},
	} {
		t.Run(fmt.Sprintf("%s/unsupportedPK=%t", tc.strategy, tc.fallback), func(t *testing.T) {
			m, r, w := newFullScanTest(tc.strategy, "MixedCase", "Empty")
			r.withoutRange, r.rows["Empty"] = tc.fallback, nil
			report := m.Run(context.Background())
			require.Equal(t, 2, report.Data.Success)
			require.Len(t, report.RowCounts, 2)
			for _, rc := range report.RowCounts {
				require.True(t, rc.Match)
			}
			require.Equal(t, []int{tc.counts, tc.pages, tc.plans, tc.streams, tc.ranges}, []int{r.counts, r.pages, r.plans, r.streams, r.ranges})
			sort.Ints(w.data["mixedcase"])
			require.Equal(t, []int{1, 2, 3, 4, 5, 6}, w.data["mixedcase"])
		})
	}
}

func TestMySQLFullScanFailureIsolatesTable(t *testing.T) {
	for _, strategy := range []string{"offset", "stream", "range"} {
		for _, failure := range []string{"read", "write"} {
			t.Run(strategy+"/"+failure, func(t *testing.T) {
				m, r, w := newFullScanTest(strategy, "Bad", "Good")
				if failure == "read" {
					r.failRead = "Bad"
				} else {
					w.failOnTable = "bad"
				}
				report := m.Run(context.Background())
				require.Equal(t, 1, report.Data.Failed)
				require.Equal(t, 1, report.Data.Success)
				require.Len(t, report.Data.Items, 1)
				require.Equal(t, "Bad", report.Data.Items[0].Name)
				require.Contains(t, report.Data.Items[0].Error, "invalid row")
				require.Len(t, report.RowCounts, 1)
				require.True(t, report.RowCounts[0].Match)
				sort.Ints(w.data["good"])
				require.Equal(t, []int{1, 2, 3, 4, 5, 6}, w.data["good"])
			})
		}
	}
}

func TestMySQLFullScanConcurrencyAndCancel(t *testing.T) {
	for _, strategy := range []string{"offset", "stream", "range"} {
		t.Run(strategy, func(t *testing.T) {
			m, _, w := newFullScanTest(strategy, "MixedCase")
			w.started, w.proceed = make(chan struct{}, 8), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan bool, 1)
			go func() { ok, _ := m.migrateTableData(ctx, "MixedCase"); result <- ok }()
			for range 2 {
				select {
				case <-w.started:
				case <-time.After(3 * time.Second):
					t.Fatal("the table did not utilize its two write slots")
				}
			}
			cancel()
			select {
			case ok := <-result:
				require.False(t, ok)
			case <-time.After(3 * time.Second):
				t.Fatal("source or writer did not exit after cancellation")
			}
			require.Equal(t, 2, w.peak)
			require.Zero(t, w.active)
		})
	}
}

func TestMySQLFullScanCDCUsesOriginalPagination(t *testing.T) {
	m, r, _ := newFullScanTest("range", "T")
	m.cfg.MigrationPurpose = "cdc"
	ok, reason := m.migrateTableData(context.Background(), "T")
	require.True(t, ok, reason)
	require.Equal(t, 3, r.pages)
	require.Zero(t, r.plans+r.streams+r.ranges)
}
