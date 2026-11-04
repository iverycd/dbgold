package datamigrate

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

// All times are milliseconds. ReadMS and WriteMS are sums of worker times,
// which overlap each other and may exceed the wall time.
type scanTrialTable struct {
	QueueMS, WallMS, FinishMS, ReadMS, WriteMS float64
	RowsWritten, VerifiedTargetRows            int64
	OffsetPages                                int               `json:",omitempty"`
	OffsetThirdsMS                             []float64         `json:",omitempty"`
	Streams                                    []scanTrialStream `json:",omitempty"`
	pages                                      []scanPageTime
}

type scanTrialStream struct {
	Lower, Upper               string `json:",omitempty"`
	Rows                       int
	WallMS, ReadMS, ConsumerMS float64
}

type scanPageTime struct {
	offset int64
	ms     float64
}

type scanTrialPool struct {
	Waits  int64
	WaitMS float64
}

func poolDelta(start, end sql.DBStats) scanTrialPool {
	return scanTrialPool{end.WaitCount - start.WaitCount, scanMS(end.WaitDuration - start.WaitDuration)}
}

func scanMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

type scanTrialResult struct {
	Round                  int
	Strategy               string
	Target                 string
	Group                  string `json:",omitempty"`
	WriteStrategy          string `json:",omitempty"`
	BatchExecs             int64
	ScalarRows             int64
	JobHasFailures         bool
	ContentValidation      string
	Tables                 map[string]*scanTrialTable
	CopyMS, JobMS          float64
	PeakRSSMiB             float64
	SourceRowsExamined     int64
	SourceTableFetches     int64
	SourcePool, TargetPool scanTrialPool
}

type scanTrialMetrics struct {
	mu     sync.Mutex
	start  time.Time
	tables map[string]*scanTrialTable
}

// The phase observer creates table entries before their copy worker runs.
// Lookups from metadata preparation and final validation are ignored.
func (m *scanTrialMetrics) tableBoundary(table string, start bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	elapsed := scanMS(time.Since(m.start))
	if start {
		m.tables[table] = &scanTrialTable{QueueMS: elapsed}
	} else {
		row := m.tables[table]
		row.FinishMS, row.WallMS = elapsed, elapsed-row.QueueMS
		// Offset groups are determined by logical page position, regardless
		// of concurrent completion order. For short tables, show one mean.
		sort.Slice(row.pages, func(i, j int) bool { return row.pages[i].offset < row.pages[j].offset })
		n := len(row.pages)
		row.OffsetPages = n
		groups := 1
		if n >= 3 {
			groups = 3
		}
		for g := 0; n > 0 && g < groups; g++ {
			from, to := n*g/groups, n*(g+1)/groups
			var total float64
			for _, page := range row.pages[from:to] {
				total += page.ms
			}
			row.OffsetThirdsMS = append(row.OffsetThirdsMS, total/float64(to-from))
		}
	}
}

func (m *scanTrialMetrics) record(table string, fn func(*scanTrialTable)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row := m.tables[table]; row != nil && row.FinishMS == 0 {
		fn(row)
	}
}

type scanTrialSource struct {
	*source.MySQLReader
	metrics *scanTrialMetrics
}

func (r *scanTrialSource) ReadPage(ctx context.Context, table string, keys []string, offset, limit int64) ([]string, []string, [][]any, error) {
	start := time.Now()
	cols, types, rows, err := r.MySQLReader.ReadPage(ctx, table, keys, offset, limit)
	ms := scanMS(time.Since(start))
	r.metrics.record(table, func(t *scanTrialTable) {
		t.ReadMS += ms
		t.pages = append(t.pages, scanPageTime{offset, ms})
	})
	return cols, types, rows, err
}

func (r *scanTrialSource) CountRows(ctx context.Context, table string) (int64, error) {
	start := time.Now()
	count, err := r.MySQLReader.CountRows(ctx, table)
	ms := scanMS(time.Since(start))
	r.metrics.record(table, func(t *scanTrialTable) { t.ReadMS += ms })
	return count, err
}

func (r *scanTrialSource) GetPrimaryKey(ctx context.Context, table string) ([]string, error) {
	start := time.Now()
	keys, err := r.MySQLReader.GetPrimaryKey(ctx, table)
	ms := scanMS(time.Since(start))
	r.metrics.record(table, func(t *scanTrialTable) { t.ReadMS += ms })
	return keys, err
}

func (r *scanTrialSource) MySQLScanRanges(ctx context.Context, table string, parts int) ([]source.MySQLScanRange, error) {
	start := time.Now()
	spans, err := r.MySQLReader.MySQLScanRanges(ctx, table, parts)
	ms := scanMS(time.Since(start))
	r.metrics.record(table, func(t *scanTrialTable) { t.ReadMS += ms })
	return spans, err
}

func (r *scanTrialSource) StreamMySQLTable(ctx context.Context, table string, span *source.MySQLScanRange, size int, consume func([]string, []string, [][]any) error) error {
	start := time.Now()
	var sample scanTrialStream
	if span != nil {
		sample.Lower, sample.Upper = span.Lower, span.Upper
	}
	err := r.MySQLReader.StreamMySQLTable(ctx, table, span, size, func(cols, types []string, rows [][]any) error {
		now := time.Now()
		err := consume(cols, types, rows)
		sample.ConsumerMS += scanMS(time.Since(now))
		sample.Rows += len(rows)
		return err
	})
	sample.WallMS = scanMS(time.Since(start))
	sample.ReadMS = sample.WallMS - sample.ConsumerMS
	r.metrics.record(table, func(t *scanTrialTable) {
		t.ReadMS += sample.ReadMS
		t.Streams = append(t.Streams, sample)
	})
	return err
}

type scanTrialTarget struct {
	*target.GaussDBWriter
	metrics *scanTrialMetrics
}

func (m *scanTrialMetrics) recordWrite(table string, start time.Time, rows int, err error) {
	ms := scanMS(time.Since(start))
	m.record(table, func(t *scanTrialTable) {
		t.WriteMS += ms
		if err == nil {
			t.RowsWritten += int64(rows)
		}
	})
}

func (w *scanTrialTarget) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	start := time.Now()
	err := w.GaussDBWriter.CopyData(ctx, table, cols, types, rows)
	w.metrics.recordWrite(table, start, len(rows), err)
	return err
}

type scanTrialDMTarget struct {
	*target.DaMengWriter
	metrics *scanTrialMetrics
}

func (w *scanTrialDMTarget) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	start := time.Now()
	err := w.DaMengWriter.CopyData(ctx, table, cols, types, rows)
	w.metrics.recordWrite(table, start, len(rows), err)
	return err
}

func (w *scanTrialDMTarget) CopyMySQLBatchData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	start := time.Now()
	err := w.DaMengWriter.CopyMySQLBatchData(ctx, table, cols, types, rows)
	w.metrics.recordWrite(table, start, len(rows), err)
	return err
}
