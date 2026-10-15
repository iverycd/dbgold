package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type metadataPreparationReader struct {
	*metadataCountingReader
	limit int
}

func (r *metadataPreparationReader) MetadataReadConcurrency() int { return r.limit }

type metadataPreparationOutcome struct {
	results map[string]sourceTableDDLResult
	err     error
}

func awaitMetadataStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case table := <-started:
		return table
	case <-time.After(5 * time.Second):
		t.Fatal("metadata worker did not start")
		return ""
	}
}

func awaitMetadataPreparation(t *testing.T, done <-chan metadataPreparationOutcome) metadataPreparationOutcome {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("metadata preparation did not finish")
		return metadataPreparationOutcome{}
	}
}

func TestPrewarmSourceTableDDLBoundsConcurrencyAndPreservesNames(t *testing.T) {
	for _, limit := range []int{2, 4, 9} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			started := make(chan string, 8)
			release := make(chan struct{})
			var active, peak atomic.Int32
			r := &metadataPreparationReader{limit: limit, metadataCountingReader: &metadataCountingReader{
				mockReader: &mockReader{},
				load: func(_ context.Context, table string, _ int) (*source.TableDDLInfo, error) {
					n := active.Add(1)
					for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
					}
					started <- table
					<-release
					active.Add(-1)
					return &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: "MixedID"}}}, nil
				},
			}}
			m, _ := newTestMigrator(r, &mockWriter{})
			m.resetSourceTableDDLCache()
			m.cfg.ExcludedObjects = []ObjectRef{{Kind: "table", Name: "Excluded"}, {Kind: "index", Name: "Other"}}
			done := make(chan metadataPreparationOutcome, 1)
			go func() {
				results, err := m.prewarmSourceTableDDL(context.Background(), []string{"Mixed", "mixed", "Other", "Fourth", "Fifth", "Excluded", "Mixed"}, nil)
				done <- metadataPreparationOutcome{results, err}
			}()
			parallel := min(4, limit)
			for range parallel {
				awaitMetadataStart(t, started)
			}
			select {
			case table := <-started:
				t.Errorf("worker exceeded concurrency limit: %s", table)
			default:
			}
			close(release)
			outcome := awaitMetadataPreparation(t, done)
			require.NoError(t, outcome.err)
			require.Len(t, outcome.results, 5)
			require.Equal(t, int32(parallel), peak.Load())
			require.Zero(t, active.Load())
			require.Zero(t, r.callCount("Excluded"))
			for _, table := range []string{"Mixed", "mixed", "Other", "Fourth", "Fifth"} {
				require.Equal(t, table, outcome.results[table].info.TableName)
				info, err := m.sourceTableDDLInfo(context.Background(), table)
				require.NoError(t, err)
				require.Equal(t, "MixedID", info.Columns[0].Name)
				require.Equal(t, 1, r.callCount(table))
			}
		})
	}
}

func TestPrewarmSourceTableDDLSerialFallbackDoesNotRead(t *testing.T) {
	for _, test := range []struct {
		name   string
		reader source.Reader
	}{
		{"unknown", &metadataCountingReader{mockReader: &mockReader{}}},
		{"limit_one", &metadataPreparationReader{limit: 1, metadataCountingReader: &metadataCountingReader{mockReader: &mockReader{}}}},
		{"limit_zero", &metadataPreparationReader{limit: 0, metadataCountingReader: &metadataCountingReader{mockReader: &mockReader{}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, _ := newTestMigrator(test.reader, &mockWriter{})
			results, err := m.prewarmSourceTableDDL(context.Background(), []string{"A", "B"}, func(_, _ int) { t.Error("serial fallback must not report prewarming") })
			require.NoError(t, err)
			require.Nil(t, results)
			var calls map[string]int
			switch r := test.reader.(type) {
			case *metadataPreparationReader:
				calls = r.calls
			case *metadataCountingReader:
				calls = r.calls
			}
			require.Empty(t, calls)
		})
	}
}

func TestPrewarmSourceTableDDLReportsProgressBeforeAllReadsFinish(t *testing.T) {
	started := make(chan string, 3)
	progress := make(chan int, 4)
	gates := map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{}), "C": make(chan struct{})}
	r := &metadataPreparationReader{limit: 2, metadataCountingReader: &metadataCountingReader{
		mockReader: &mockReader{},
		load: func(_ context.Context, table string, _ int) (*source.TableDDLInfo, error) {
			started <- table
			<-gates[table]
			return &source.TableDDLInfo{TableName: table}, nil
		},
	}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	done := make(chan metadataPreparationOutcome, 1)
	go func() {
		results, err := m.prewarmSourceTableDDL(context.Background(), []string{"A", "B", "C"}, func(done, total int) {
			progress <- done
		})
		done <- metadataPreparationOutcome{results, err}
	}()
	awaitMetadataStart(t, started)
	awaitMetadataStart(t, started)
	require.Equal(t, 0, <-progress)
	close(gates["A"])
	select {
	case completed := <-progress:
		require.Equal(t, 1, completed)
	case <-time.After(5 * time.Second):
		t.Fatal("progress was withheld while other metadata reads were still running")
	}
	select {
	case <-done:
		t.Error("preparation finished before all table metadata was read")
	default:
	}
	close(gates["B"])
	close(gates["C"])
	outcome := awaitMetadataPreparation(t, done)
	require.NoError(t, outcome.err)
	require.Len(t, outcome.results, 3)
	require.Equal(t, 2, <-progress)
	require.Equal(t, 3, <-progress)
}

func TestPrewarmSourceTableDDLPreservesErrorsNilAndDeepCopies(t *testing.T) {
	readErr := errors.New("metadata unavailable")
	defaultValue, last := "Original", int64(19)
	original := &source.TableDDLInfo{TableName: "Good", Columns: []source.ColumnInfo{{Name: "MixedID", Default: &defaultValue, IdentityInfo: &source.IdentityInfo{Last: &last}}}}
	r := &metadataPreparationReader{limit: 4, metadataCountingReader: &metadataCountingReader{
		mockReader: &mockReader{},
		load: func(_ context.Context, table string, count int) (*source.TableDDLInfo, error) {
			if count == 1 {
				switch table {
				case "Error":
					return nil, readErr
				case "Nil":
					return nil, nil
				case "Good":
					return original, nil
				}
			}
			return &source.TableDDLInfo{TableName: table}, nil
		},
	}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	var progress []int
	results, err := m.prewarmSourceTableDDL(context.Background(), []string{"Good", "Error", "Nil"}, func(done, total int) {
		require.Equal(t, 3, total)
		progress = append(progress, done)
	})
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, progress)
	require.ErrorIs(t, results["Error"].err, readErr)
	require.Nil(t, results["Nil"].info)
	require.NoError(t, results["Nil"].err)
	require.Len(t, m.sourceDDLCache.tables, 1)
	for _, table := range []string{"Good", "Error", "Nil"} {
		require.Equal(t, 1, r.callCount(table), "preparation must preserve its first result without retrying")
	}
	original.Columns[0].Name = "Reader mutation"
	*original.Columns[0].Default = "Reader mutation"
	*original.Columns[0].IdentityInfo.Last = 100
	require.Equal(t, "MixedID", results["Good"].info.Columns[0].Name)
	require.Equal(t, "Original", *results["Good"].info.Columns[0].Default)
	require.Equal(t, int64(19), *results["Good"].info.Columns[0].IdentityInfo.Last)
	results["Good"].info.Columns[0].Name = "Caller mutation"
	info, err := m.sourceTableDDLInfo(context.Background(), "Good")
	require.NoError(t, err)
	require.Equal(t, "MixedID", info.Columns[0].Name)
	require.Equal(t, 1, r.callCount("Good"))
	for _, table := range []string{"Error", "Nil"} {
		info, err := m.sourceTableDDLInfo(context.Background(), table)
		require.NoError(t, err)
		require.Equal(t, table, info.TableName)
		require.Equal(t, 2, r.callCount(table), "later migration stages can retry unsuccessful metadata")
	}
}

func TestPrewarmSourceTableDDLCancellationWaitsAndDoesNotCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, cancelled := make(chan string, 8), make(chan string, 8)
	release := make(chan struct{})
	r := &metadataPreparationReader{limit: 4, metadataCountingReader: &metadataCountingReader{
		mockReader: &mockReader{},
		load: func(ctx context.Context, table string, _ int) (*source.TableDDLInfo, error) {
			started <- table
			<-ctx.Done()
			cancelled <- table
			<-release
			return &source.TableDDLInfo{TableName: table}, nil
		},
	}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	done := make(chan metadataPreparationOutcome, 1)
	go func() {
		results, err := m.prewarmSourceTableDDL(ctx, []string{"A", "B", "C", "D", "E", "F"}, nil)
		done <- metadataPreparationOutcome{results, err}
	}()
	for range 4 {
		awaitMetadataStart(t, started)
	}
	cancel()
	for range 4 {
		awaitMetadataStart(t, cancelled)
	}
	select {
	case <-done:
		t.Error("preparation returned before in-flight metadata readers exited")
	default:
	}
	close(release)
	outcome := awaitMetadataPreparation(t, done)
	require.ErrorIs(t, outcome.err, context.Canceled)
	require.Nil(t, outcome.results)
	require.Empty(t, m.sourceDDLCache.tables)
	require.Zero(t, r.callCount("E"))
	require.Zero(t, r.callCount("F"))
	results, err := m.prewarmSourceTableDDL(ctx, []string{"A", "B"}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, results)
	require.Equal(t, 1, r.callCount("A"))
}

func TestPlanDamengTableReusesSourceCacheAndKeepsSnapshotFallback(t *testing.T) {
	r := &metadataCountingReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{
		"Mixed":   {TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "MixedID", DataType: "int"}}},
		"Outside": {TableName: "Outside", Columns: []source.ColumnInfo{{Name: "OutsideID", DataType: "int"}}},
	}}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	_, err := m.sourceTableDDLInfo(context.Background(), "Mixed")
	require.NoError(t, err)
	plan, err := m.planDamengTable(context.Background(), "Mixed")
	require.NoError(t, err)
	require.Equal(t, "MixedID", plan.source.Columns[0].Name)
	require.Equal(t, 1, r.callCount("Mixed"))
	m.reader = &sqlServerDamengSnapshot{Reader: r, tables: map[string]*source.TableDDLInfo{}}
	plan, err = m.planDamengTable(context.Background(), "Outside")
	require.NoError(t, err)
	require.Equal(t, "OutsideID", plan.source.Columns[0].Name)
	require.Equal(t, 1, r.callCount("Outside"))
}

func newMetadataPreparationRunFixture(limit int, delay time.Duration) (*Migrator, *metadataPreparationReader, *mockWriter) {
	base := &mockReader{ddl: make(map[string]*source.TableDDLInfo, 118)}
	for i := range 118 {
		prefix := []string{"MixedTable", "mixedTable", "MIXED_TABLE"}[i%3]
		table := fmt.Sprintf("%s%03d", prefix, i)
		base.tables = append(base.tables, table)
		base.ddl[table] = &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{
			{Name: "MixedID", DataType: "int"},
			{Name: "MixedText", DataType: "varchar", Length: 128, IsNullable: true},
		}}
	}
	r := &metadataPreparationReader{limit: limit, metadataCountingReader: &metadataCountingReader{mockReader: base}}
	if delay > 0 {
		r.load = func(ctx context.Context, table string, _ int) (*source.TableDDLInfo, error) {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
				return base.ddl[table], nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	w := &mockWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.CaptureRepair, m.cfg.Content, m.cfg.LowerCaseNames = true, "schema_only", true
	m.cfg.TargetSchema, m.cfg.TargetDBType = "dst", "postgres"
	return m, r, w
}

func TestMetadataPreparationRun118TablesMatchesSerialManifestAndDDL(t *testing.T) {
	var serialReport MigrationReport
	var serialDDL []string
	for _, limit := range []int{1, 4} {
		m, r, w := newMetadataPreparationRunFixture(limit, 0)
		report := m.Run(context.Background())
		require.False(t, report.HasFailures(), "concurrency %d", limit)
		require.Equal(t, 118, report.Tables.Success)
		require.NotNil(t, report.RepairManifest)
		require.Len(t, report.RepairManifest.Objects, 118)
		require.Len(t, w.created, 118)
		require.Empty(t, w.copied)
		require.Nil(t, m.sourceDDLCache.tables)
		for i, table := range r.tables {
			object := report.RepairManifest.Objects[i]
			require.Equal(t, "table", object.Kind)
			require.Equal(t, table, object.SourceName)
			require.Equal(t, strings.ToLower(table), object.TargetName)
			require.Equal(t, "MixedID", object.SourceColumns[0].Name)
			require.Equal(t, "mixedid", object.Columns[0].Name)
			require.Equal(t, w.created[i], object.DDL)
			require.Equal(t, 1, r.callCount(table), "%s must be read once at concurrency %d", table, limit)
			require.Equal(t, "MixedID", r.ddl[table].Columns[0].Name)
		}
		if limit == 1 {
			serialReport, serialDDL = report, w.created
		} else {
			require.Equal(t, serialReport, report, "parallel preparation must preserve the complete final report and manifest")
			require.Equal(t, serialDDL, w.created, "target DDL and execution order must remain identical")
		}
	}
}

func TestMetadataPreparationBeginRepairDoesNotRetryErrorOrNilBeforeNextRun(t *testing.T) {
	for _, limit := range []int{1, 4} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			readErr := errors.New("temporary metadata error")
			r := &metadataPreparationReader{limit: limit, metadataCountingReader: &metadataCountingReader{
				mockReader: &mockReader{tables: []string{"Good", "Error", "Nil"}},
				load: func(_ context.Context, table string, count int) (*source.TableDDLInfo, error) {
					if count == 1 {
						switch table {
						case "Error":
							return nil, readErr
						case "Nil":
							return nil, nil
						}
					}
					return &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: "MixedID", DataType: "int"}}}, nil
				},
			}}
			w := &mockWriter{}
			m, _ := newTestMigrator(r, w)
			m.cfg.CaptureRepair, m.cfg.Content = true, "schema_only"
			m.resetSourceTableDDLCache()
			m.beginRepair(context.Background())
			require.NoError(t, m.repairs.sinkErr)
			require.Len(t, m.repairs.objects, 3)
			require.Equal(t, readErr.Error(), m.repairs.objects[1].Error)
			require.Empty(t, m.repairs.objects[1].DDL)
			require.Empty(t, m.repairs.objects[2].DDL)
			require.Len(t, m.sourceDDLCache.tables, 1)
			require.Empty(t, w.created)
			for _, table := range r.tables {
				require.Equal(t, 1, r.callCount(table), "beginRepair must consume each first result without a retry")
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			require.Equal(t, 3, report.Tables.Success)
			require.Len(t, w.created, 3)
			require.Nil(t, m.sourceDDLCache.tables)
			for _, table := range r.tables {
				require.Equal(t, 2, r.callCount(table), "a new Run must read fresh metadata and recover")
			}
		})
	}
}

func TestMetadataPreparationRunCancellationPreventsDDLAndClearsCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, cancelled := make(chan string, 4), make(chan string, 4)
	release := make(chan struct{})
	r := &metadataPreparationReader{limit: 4, metadataCountingReader: &metadataCountingReader{
		mockReader: &mockReader{tables: []string{"A", "B", "C", "D", "E", "F"}},
		load: func(ctx context.Context, table string, _ int) (*source.TableDDLInfo, error) {
			started <- table
			<-ctx.Done()
			cancelled <- table
			<-release
			return &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}}, nil
		},
	}}
	w := &mockWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.CaptureRepair, m.cfg.Content = true, "schema_only"
	done := make(chan MigrationReport, 1)
	go func() { done <- m.Run(ctx) }()
	for range 4 {
		awaitMetadataStart(t, started)
	}
	cancel()
	for range 4 {
		awaitMetadataStart(t, cancelled)
	}
	select {
	case <-done:
		t.Error("Run must wait for active metadata readers before clearing its cache")
	default:
	}
	close(release)
	select {
	case report := <-done:
		require.True(t, report.HasFailures())
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled migration did not finish")
	}
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
	require.Nil(t, m.sourceDDLCache.tables)
	require.Zero(t, r.callCount("E"))
	require.Zero(t, r.callCount("F"))
}

// This benchmark isolates preparation with a controlled one-millisecond delay
// per source table read; its speedup is not a prediction for a live database.
func BenchmarkMetadataPreparation118Tables(b *testing.B) {
	for _, limit := range []int{1, 4} {
		b.Run(fmt.Sprintf("concurrency_%d", limit), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				m, r, _ := newMetadataPreparationRunFixture(limit, time.Millisecond)
				m.resetSourceTableDDLCache()
				b.StartTimer()
				m.beginRepair(context.Background())
				b.StopTimer()
				if m.repairs.sinkErr != nil {
					b.Fatal(m.repairs.sinkErr)
				}
				if len(m.repairs.objects) != 118 {
					b.Fatalf("prepared %d objects, want 118", len(m.repairs.objects))
				}
				for _, table := range r.tables {
					if count := r.callCount(table); count != 1 {
						b.Fatalf("%s read %d times, want 1", table, count)
					}
				}
			}
			b.ReportMetric(118, "tables/op")
		})
	}
}
