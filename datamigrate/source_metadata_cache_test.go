package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type metadataCountingReader struct {
	*mockReader
	mu    sync.Mutex
	calls map[string]int
	load  func(context.Context, string, int) (*source.TableDDLInfo, error)
}

func (r *metadataCountingReader) GetTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = make(map[string]int)
	}
	r.calls[table]++
	count := r.calls[table]
	r.mu.Unlock()
	if r.load != nil {
		return r.load(ctx, table, count)
	}
	return r.mockReader.GetTableDDLInfo(ctx, table)
}

func (r *metadataCountingReader) callCount(table string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[table]
}

type metadataGaussWriter struct{ mockWriter }

func (w *metadataGaussWriter) DBType() string { return "gaussdb" }
func (w *metadataGaussWriter) Dialect() dialect.Dialect {
	return dialect.NewPostgres("gaussdb")
}

func TestSourceTableDDLCacheReads118TablesOncePerRun(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture_repair=%t", capture), func(t *testing.T) {
			r := &metadataCountingReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{}}}
			for i := 0; i < 118; i++ {
				table := fmt.Sprintf("MixedTable%d", i)
				r.tables = append(r.tables, table)
				r.ddl[table] = &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: "MixedID", DataType: "int"}}}
			}
			w := &metadataGaussWriter{}
			m, _ := newTestMigrator(r, w)
			m.cfg.CaptureRepair = capture
			m.cfg.Content, m.cfg.TargetSchema, m.cfg.TargetDBType = "schema_only", "dst", "gaussdb"
			m.cfg.LowerCaseNames = true

			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			require.Equal(t, 118, report.Tables.Success)
			require.Len(t, w.created, 118)
			for _, table := range r.tables {
				require.Equal(t, 1, r.callCount(table), "source structure should be read once for %s", table)
				require.Equal(t, "MixedID", r.ddl[table].Columns[0].Name)
			}
			require.Contains(t, w.created[0], `CREATE TABLE "dst"."mixedtable0"`)
			require.Contains(t, w.created[0], `"mixedid" int NOT NULL`)
			require.Nil(t, m.sourceDDLCache.tables, "release source metadata after Run")
			if capture {
				require.NotNil(t, report.RepairManifest)
				found := false
				for _, object := range report.RepairManifest.Objects {
					if object.Kind == "table" && object.SourceTable == "MixedTable0" {
						found = true
						require.Equal(t, "MixedID", object.SourceColumns[0].Name)
						require.Equal(t, "mixedid", object.Columns[0].Name)
						require.Equal(t, w.created[0], object.DDL)
					}
				}
				require.True(t, found)
			}
		})
	}
}

func TestSourceTableDDLCacheRepeatedRunReloadsSource(t *testing.T) {
	r := &metadataCountingReader{mockReader: &mockReader{
		tables: []string{"Mixed"},
		ddl:    map[string]*source.TableDDLInfo{"Mixed": {TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "Before", DataType: "int"}}}},
	}}
	w := &metadataGaussWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.CaptureRepair, m.cfg.Content = true, "schema_only"
	first := m.Run(context.Background())
	require.False(t, first.HasFailures())
	require.Nil(t, m.sourceDDLCache.tables)
	r.ddl["Mixed"].Columns[0].Name = "After"
	second := m.Run(context.Background())
	require.False(t, second.HasFailures())
	require.Equal(t, 2, r.callCount("Mixed"))
	require.Contains(t, w.created[0], `"Before" int NOT NULL`)
	require.Contains(t, w.created[1], `"After" int NOT NULL`)
	require.Nil(t, m.sourceDDLCache.tables)
}

func TestSourceTableDDLCacheCancelledRunCleansUpAndCanReload(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture_repair=%t", capture), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &metadataCountingReader{
				mockReader: &mockReader{tables: []string{"Mixed"}},
				load: func(_ context.Context, table string, count int) (*source.TableDDLInfo, error) {
					column := "FreshRead"
					if count == 1 {
						cancel()
						column = "CancelledRead"
					}
					return &source.TableDDLInfo{TableName: table, Columns: []source.ColumnInfo{{Name: column, DataType: "int"}}}, nil
				},
			}
			w := &metadataGaussWriter{}
			m, _ := newTestMigrator(r, w)
			m.cfg.CaptureRepair, m.cfg.Content = capture, "schema_only"

			cancelledReport := m.Run(ctx)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Zero(t, cancelledReport.Tables.Success)
			require.Equal(t, 1, r.callCount("Mixed"))
			require.Nil(t, m.sourceDDLCache.tables, "Run must release the cache on cancellation")
			require.Empty(t, w.created, "cancelled metadata must not reach target DDL execution")

			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			require.Equal(t, 1, report.Tables.Success)
			require.Equal(t, 2, r.callCount("Mixed"), "a fresh Run must query the Reader again")
			require.Len(t, w.created, 1)
			require.Contains(t, w.created[0], `"FreshRead" int NOT NULL`)
			require.NotContains(t, w.created[0], "CancelledRead")
			require.Nil(t, m.sourceDDLCache.tables)
		})
	}
}

func TestSourceTableDDLCachePreservesCaseAndDeepCopies(t *testing.T) {
	defaultValue, last := "Original", int64(19)
	original := &source.TableDDLInfo{TableName: "Mixed", Comment: "Original table", Columns: []source.ColumnInfo{{
		Name: "MixedID", DataType: "int", Default: &defaultValue,
		IdentityInfo: &source.IdentityInfo{Seed: 3, Increment: 2, Last: &last},
	}}}
	r := &metadataCountingReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{
		"Mixed": original,
		"mixed": {TableName: "mixed", Columns: []source.ColumnInfo{{Name: "LowerID", DataType: "bigint"}}},
	}}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	first, err := m.sourceTableDDLInfo(context.Background(), "Mixed")
	require.NoError(t, err)
	first.TableName, first.Comment, first.Columns[0].Name, first.Columns[0].DataType = "Changed", "Changed", "Changed", "varchar"
	*first.Columns[0].Default = "Changed"
	first.Columns[0].IdentityInfo.Seed = 100
	*first.Columns[0].IdentityInfo.Last = 100
	require.Equal(t, "MixedID", original.Columns[0].Name)
	require.Equal(t, "Original", *original.Columns[0].Default)
	require.Equal(t, int64(3), original.Columns[0].IdentityInfo.Seed)
	require.Equal(t, int64(19), *original.Columns[0].IdentityInfo.Last)

	// The cached snapshot must also be independent of the Reader's own object.
	original.Columns[0].Name = "Reader changed"
	*original.Columns[0].Default = "Reader changed"
	original.Columns[0].IdentityInfo.Seed = 200
	*original.Columns[0].IdentityInfo.Last = 200
	second, err := m.sourceTableDDLInfo(context.Background(), "Mixed")
	require.NoError(t, err)
	require.Equal(t, "Mixed", second.TableName)
	require.Equal(t, "Original table", second.Comment)
	require.Equal(t, "MixedID", second.Columns[0].Name)
	require.Equal(t, "int", second.Columns[0].DataType)
	require.Equal(t, "Original", *second.Columns[0].Default)
	require.Equal(t, int64(3), second.Columns[0].IdentityInfo.Seed)
	require.Equal(t, int64(19), *second.Columns[0].IdentityInfo.Last)
	*second.Columns[0].Default = "Second caller changed"
	third, err := m.sourceTableDDLInfo(context.Background(), "Mixed")
	require.NoError(t, err)
	require.Equal(t, "Original", *third.Columns[0].Default)
	lower, err := m.sourceTableDDLInfo(context.Background(), "mixed")
	require.NoError(t, err)
	require.Equal(t, "LowerID", lower.Columns[0].Name)
	require.Equal(t, 1, r.callCount("Mixed"))
	require.Equal(t, 1, r.callCount("mixed"))
}

func TestSourceTableDDLCacheRetriesErrorsAndNilResults(t *testing.T) {
	readErr := errors.New("temporary metadata failure")
	r := &metadataCountingReader{mockReader: &mockReader{}, load: func(_ context.Context, table string, count int) (*source.TableDDLInfo, error) {
		switch count {
		case 1:
			return nil, readErr
		case 2:
			return nil, nil
		default:
			return &source.TableDDLInfo{TableName: table}, nil
		}
	}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	_, err := m.sourceTableDDLInfo(context.Background(), "T")
	require.ErrorIs(t, err, readErr)
	info, err := m.sourceTableDDLInfo(context.Background(), "T")
	require.NoError(t, err)
	require.Nil(t, info)
	for i := 0; i < 2; i++ {
		info, err = m.sourceTableDDLInfo(context.Background(), "T")
		require.NoError(t, err)
		require.Equal(t, "T", info.TableName)
	}
	require.Equal(t, 3, r.callCount("T"))
}

func TestSourceTableDDLCacheDoesNotCacheCancelledReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &metadataCountingReader{mockReader: &mockReader{}, load: func(_ context.Context, table string, count int) (*source.TableDDLInfo, error) {
		if count == 1 {
			cancel()
		}
		return &source.TableDDLInfo{TableName: table}, nil
	}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	info, err := m.sourceTableDDLInfo(ctx, "T")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, info)
	info, err = m.sourceTableDDLInfo(context.Background(), "T")
	require.NoError(t, err)
	require.Equal(t, "T", info.TableName)
	require.Equal(t, 2, r.callCount("T"))
	for _, table := range []string{"T", "Uncached"} {
		info, err = m.sourceTableDDLInfo(ctx, table)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, info)
	}
	require.Equal(t, 2, r.callCount("T"))
	require.Zero(t, r.callCount("Uncached"))
}

func TestSourceTableDDLCacheConcurrentCallsReadOnce(t *testing.T) {
	r := &metadataCountingReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{
		"T": {TableName: "T", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}},
	}}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.resetSourceTableDDLCache()
	const count = 12
	results := make(chan *source.TableDDLInfo, count)
	errors := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := m.sourceTableDDLInfo(context.Background(), "T")
			results <- info
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	for info := range results {
		require.Equal(t, "ID", info.Columns[0].Name)
		info.Columns[0].Name = "Caller mutation"
	}
	require.Equal(t, 1, r.callCount("T"))
	require.Equal(t, "ID", r.ddl["T"].Columns[0].Name)
}

func TestSourceTableDDLInfoOutsideRunDoesNotCache(t *testing.T) {
	r := &metadataCountingReader{mockReader: &mockReader{ddl: map[string]*source.TableDDLInfo{
		"T": {TableName: "T", Columns: []source.ColumnInfo{{Name: "Before", DataType: "int"}}},
	}}}
	m, _ := newTestMigrator(r, &mockWriter{})
	first, err := m.sourceTableDDLInfo(context.Background(), "T")
	require.NoError(t, err)
	first.Columns[0].Name = "Caller mutation"
	require.Equal(t, "Before", r.ddl["T"].Columns[0].Name)
	r.ddl["T"].Columns[0].Name = "After"
	second, err := m.sourceTableDDLInfo(context.Background(), "T")
	require.NoError(t, err)
	require.Equal(t, "After", second.Columns[0].Name)
	require.Equal(t, 2, r.callCount("T"))
	require.Nil(t, m.sourceDDLCache.tables)
}

func TestBuildCreateTableDDLStillUsesDamengTargetPlan(t *testing.T) {
	r := &metadataCountingReader{mockReader: &mockReader{}}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.dmPlans = map[string]*damengTablePlan{"Mixed": {
		target: &source.TableDDLInfo{TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "Renamed", DataType: "int"}}},
	}}
	_, ddl, err := m.buildCreateTableDDL(context.Background(), "Mixed")
	require.NoError(t, err)
	require.Contains(t, ddl, `"Renamed" int NOT NULL`)
	require.Zero(t, r.callCount("Mixed"))
	require.Empty(t, m.sourceDDLCache.tables)
}
