package main

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"golang.org/x/time/rate"
)

const (
	failureEnvironment = "environment_connectivity"
	failureCapacity    = "database_capacity"
	failureCorrectness = "correctness"
)

type LedgerEntry struct {
	Sequence    int64     `json:"sequence"`
	Transaction string    `json:"transaction_id"`
	Table       string    `json:"table"`
	Action      string    `json:"action"`
	RowID       int64     `json:"row_id"`
	Committed   bool      `json:"committed"`
	CommittedAt time.Time `json:"committed_at,omitempty"`
}

type WorkloadResult struct {
	Name         string        `json:"name"`
	TargetTPS    int           `json:"target_tps"`
	Duration     time.Duration `json:"duration"`
	Committed    int64         `json:"committed"`
	RolledBack   int64         `json:"rolled_back"`
	Errors       int64         `json:"errors"`
	FailureClass string        `json:"failure_class,omitempty"`
	ErrorSamples []string      `json:"error_samples,omitempty"`
	ActualTPS    float64       `json:"actual_tps"`
	LatencyMS    []float64     `json:"latency_ms,omitempty"`
}

type ledgerWriter struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func newLedgerWriter(cfg Config, runID string) (*ledgerWriter, error) {
	path := filepath.Join(cfg.resultDir(runID), "ledger.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &ledgerWriter{f: f, w: bufio.NewWriterSize(f, 256*1024)}, nil
}

func (l *ledgerWriter) write(entry LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err = l.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (l *ledgerWriter) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		_ = l.f.Close()
		return err
	}
	return l.f.Close()
}

type LatencyMarker struct {
	Table       string    `json:"table"`
	RowID       int64     `json:"row_id"`
	CommittedAt time.Time `json:"committed_at"`
}

type stageOptions struct {
	DeferLatency bool
}

type stageExecution struct {
	Result  WorkloadResult
	Markers []LatencyMarker
}

type workloadEngine struct {
	cfg    Config
	state  *RunState
	source *sql.DB
	target *sql.DB
	ledger *ledgerWriter
	seq    atomic.Int64
}

func newWorkloadEngine(cfg Config, state *RunState, source, target *sql.DB) (*workloadEngine, error) {
	ledger, err := newLedgerWriter(cfg, state.RunID)
	if err != nil {
		return nil, err
	}
	var maxRows int64
	for _, table := range state.Tables {
		if table.Rows > maxRows {
			maxRows = table.Rows
		}
	}
	engine := &workloadEngine{cfg: cfg, state: state, source: source, target: target, ledger: ledger}
	if ledgerMax, readErr := readLedgerMax(cfg, state.RunID); readErr == nil && ledgerMax > maxRows {
		maxRows = ledgerMax
	}
	engine.seq.Store(maxRows + 10_000)
	return engine, nil
}

func readLedgerMax(cfg Config, runID string) (int64, error) {
	path := filepath.Join(cfg.resultDir(runID), "ledger.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	var maximum int64
	for scanner.Scan() {
		var entry LedgerEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Sequence > maximum {
			maximum = entry.Sequence
		}
	}
	return maximum, scanner.Err()
}

func (w *workloadEngine) close() error { return w.ledger.close() }

func (w *workloadEngine) runStage(ctx context.Context, name string, targetTPS int, duration time.Duration) WorkloadResult {
	return w.runStageWithOptions(ctx, name, targetTPS, duration, stageOptions{}).Result
}

func (w *workloadEngine) runStageWithOptions(ctx context.Context, name string, targetTPS int, duration time.Duration, options stageOptions) stageExecution {
	result := WorkloadResult{Name: name, TargetTPS: targetTPS, Duration: duration}
	if duration <= 0 {
		return stageExecution{Result: result}
	}
	started := time.Now()
	stageCtx, cancelStage := context.WithTimeout(ctx, duration)
	defer cancelStage()
	limiter := rate.NewLimiter(rate.Limit(targetTPS), max(targetTPS/2, 1))
	var committed, rolledBack, failures atomic.Int64
	var wg sync.WaitGroup
	var sampleWG sync.WaitGroup
	var markerMu sync.Mutex
	var markers []LatencyMarker
	var latencyMu sync.Mutex
	var latencies []float64
	var errorMu sync.Mutex
	var errorSamples []string
	var failureClass string
	recordError := func(err error) {
		failures.Add(1)
		errorMu.Lock()
		class := classifyWorkloadError(err)
		if failurePriority(class) > failurePriority(failureClass) {
			failureClass = class
		}
		if len(errorSamples) < 10 {
			errorSamples = append(errorSamples, err.Error())
			if len(errorSamples) == 1 {
				fmt.Printf("workload %s first database error: %v\n", name, err)
			}
		}
		errorMu.Unlock()
		if class == failureEnvironment || class == failureCorrectness {
			cancelStage()
		}
	}
	for worker := 0; worker < w.cfg.Workload.Workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(w.cfg.Profile.Seed + int64(worker)*7919 + int64(targetTPS)))
			for {
				if err := limiter.Wait(stageCtx); err != nil {
					return
				}
				seq := w.seq.Add(1)
				entry, sample, err := w.applyOperation(stageCtx, rng, seq)
				if err != nil {
					if stageCtx.Err() == nil {
						recordError(err)
					}
					continue
				}
				if err = w.ledger.write(entry); err != nil {
					recordError(err)
					continue
				}
				if entry.Committed {
					committed.Add(1)
				} else {
					rolledBack.Add(1)
				}
				if sample != nil {
					if options.DeferLatency {
						markerMu.Lock()
						markers = append(markers, *sample)
						markerMu.Unlock()
					} else {
						sampleWG.Add(1)
						go func(item LatencyMarker) {
							defer sampleWG.Done()
							if latency, measureErr := w.measureLatencyUntil(ctx, item, time.Now().Add(w.cfg.Workload.CatchUpTimeout.Duration)); measureErr == nil {
								latencyMu.Lock()
								latencies = append(latencies, latency)
								latencyMu.Unlock()
							}
						}(*sample)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	elapsed := time.Since(started)
	sampleWG.Wait()
	result.Duration, result.Committed, result.RolledBack, result.Errors = elapsed, committed.Load(), rolledBack.Load(), failures.Load()
	errorMu.Lock()
	result.ErrorSamples = append([]string(nil), errorSamples...)
	result.FailureClass = failureClass
	errorMu.Unlock()
	if elapsed > 0 {
		result.ActualTPS = float64(result.Committed) / elapsed.Seconds()
	}
	latencyMu.Lock()
	result.LatencyMS = append([]float64(nil), latencies...)
	latencyMu.Unlock()
	markerMu.Lock()
	deferred := append([]LatencyMarker(nil), markers...)
	markerMu.Unlock()
	return stageExecution{Result: result, Markers: deferred}
}

func classifyWorkloadError(err error) string {
	if err == nil {
		return ""
	}
	for _, target := range []error{syscall.EADDRNOTAVAIL, syscall.EMFILE, syscall.ENFILE, syscall.ECONNREFUSED, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		if errors.Is(err, target) {
			return failureEnvironment
		}
	}
	if errors.Is(err, driver.ErrBadConn) {
		return failureEnvironment
	}
	var networkError *net.OpError
	if errors.As(err, &networkError) {
		return failureEnvironment
	}
	var mysqlError *gomysql.MySQLError
	if errors.As(err, &mysqlError) {
		switch mysqlError.Number {
		case 1040, 1205, 1213:
			return failureCapacity
		default:
			return failureCorrectness
		}
	}
	lower := strings.ToLower(err.Error())
	for _, fragment := range []string{"can't assign requested address", "too many open files", "connection refused", "network is unreachable", "no route to host"} {
		if strings.Contains(lower, fragment) {
			return failureEnvironment
		}
	}
	return failureCorrectness
}

func failurePriority(class string) int {
	switch class {
	case failureEnvironment:
		return 3
	case failureCorrectness:
		return 2
	case failureCapacity:
		return 1
	default:
		return 0
	}
}

func (w *workloadEngine) applyOperation(ctx context.Context, rng *rand.Rand, seq int64) (LedgerEntry, *LatencyMarker, error) {
	table := w.pickTable(rng)
	actionBucket := seq % 10
	action := "insert"
	if actionBucket >= 6 && actionBucket <= 7 {
		action = "update"
	}
	if actionBucket == 8 {
		action = "delete"
	}
	if actionBucket == 9 {
		action = "rollback"
	}
	txID := fmt.Sprintf("tx-%d", seq)
	entry := LedgerEntry{Sequence: seq, Transaction: txID, Table: table.Name, Action: action, RowID: seq}
	tx, err := w.source.BeginTx(ctx, nil)
	if err != nil {
		return entry, nil, err
	}
	defer tx.Rollback()
	var sample *LatencyMarker
	switch action {
	case "insert", "rollback":
		query, args := mysqlInsertBatch(table, w.state.RunID, seq, seq)
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			return entry, nil, err
		}
	case "update":
		row := int64(1)
		if table.Rows > 0 {
			row = 1 + seq%table.Rows
		}
		payload := fmt.Sprintf("updated/%s/%d/中文/🚀", txID, seq)
		if table.Kind == kindUnique {
			_, err = tx.ExecContext(ctx, "UPDATE "+mysqlIdent(table.Name)+" SET code=?,payload=?,event_id=? WHERE id=?", fmt.Sprintf("changed-%d", seq), payload, seq, row)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE "+mysqlIdent(table.Name)+" SET payload=?,event_id=? WHERE id=?", payload, seq, row)
		}
		entry.RowID = row
	case "delete":
		row := int64(1)
		if table.Rows > 0 {
			row = 1 + seq%table.Rows
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM "+mysqlIdent(table.Name)+" WHERE id=? LIMIT 1", row)
		entry.RowID = row
	}
	if err != nil {
		return entry, nil, err
	}
	if action == "rollback" {
		if err = tx.Rollback(); err != nil {
			return entry, nil, err
		}
		entry.Committed = false
		return entry, nil, nil
	}
	if err = tx.Commit(); err != nil {
		return entry, nil, err
	}
	entry.Committed, entry.CommittedAt = true, time.Now().UTC()
	if action == "insert" && seq%100 == 0 && (table.Kind == kindPrimary || table.Kind == kindWide || table.Kind == kindMixedCase) {
		sample = &LatencyMarker{Table: table.Name, RowID: seq, CommittedAt: entry.CommittedAt}
	}
	return entry, sample, nil
}

func (w *workloadEngine) pickTable(rng *rand.Rand) TableSpec {
	tables := sortedTablesByRows(w.state.Tables)
	limit := max(1, len(tables)/100)
	if rng.Intn(100) < 80 {
		return tables[rng.Intn(limit)]
	}
	return tables[rng.Intn(len(tables))]
}

func (w *workloadEngine) measureDeferredLatencies(ctx context.Context, markers []LatencyMarker, timeout time.Duration) ([]float64, error) {
	if len(markers) == 0 {
		return nil, nil
	}
	deadline := time.Now().Add(timeout)
	latencies := make([]float64, 0, len(markers))
	for _, marker := range markers {
		latency, err := w.measureLatencyUntil(ctx, marker, deadline)
		if err != nil {
			return nil, err
		}
		latencies = append(latencies, latency)
	}
	return latencies, nil
}

func (w *workloadEngine) measureLatencyUntil(ctx context.Context, sample LatencyMarker, deadline time.Time) (float64, error) {
	started := time.Now()
	name := sample.Table
	if w.cfg.Profile.LowerCaseNames {
		name = strings.ToLower(name)
	}
	query := "SELECT 1 FROM " + pgIdent(w.cfg.GaussDB.Schema) + "." + pgIdent(name) + " WHERE id=$1"
	for time.Now().Before(deadline) {
		var one int
		err := w.target.QueryRowContext(ctx, query, sample.RowID).Scan(&one)
		if err == nil {
			return float64(time.Since(sample.CommittedAt).Microseconds()) / 1000, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		wait := min(200*time.Millisecond, time.Until(deadline))
		if wait <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(wait):
		}
	}
	return 0, fmt.Errorf("latency marker did not become visible within %s: table=%s row_id=%d", time.Since(started).Round(time.Millisecond), sample.Table, sample.RowID)
}

func percentiles(values []float64) (p50, p95, p99 float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	at := func(p float64) float64 {
		index := int(float64(len(values)-1) * p)
		return values[index]
	}
	return at(.50), at(.95), at(.99)
}
