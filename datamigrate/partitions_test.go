package datamigrate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type partitionFlowReader struct {
	*mockReader
	specs          map[string]*source.PartitionSpec
	roots          map[string]string
	metadataErrors map[string]error
	metadataCalls  map[string]int
	metadataAt     func(string, int) *source.PartitionSpec
	rootError      error
	streamed       []string
	pageReads      map[string]int
	primary        []source.IndexInfo
	indexes        []source.IndexInfo
	fks            []source.FKInfo
	hasChecks      bool
}

func (r *partitionFlowReader) PartitionHasChecks(context.Context, string) (bool, error) {
	return r.hasChecks, nil
}

func (r *partitionFlowReader) PartitionMetadata(_ context.Context, table string) (*source.PartitionSpec, error) {
	r.metadataCalls[table]++
	if e := r.metadataErrors[table]; e != nil {
		return nil, e
	}
	if r.metadataAt != nil {
		return source.ClonePartitionSpec(r.metadataAt(table, r.metadataCalls[table])), nil
	}
	return source.ClonePartitionSpec(r.specs[table]), nil
}
func (r *partitionFlowReader) PartitionRoots(context.Context) (map[string]string, error) {
	return r.roots, r.rootError
}
func (r *partitionFlowReader) StreamPartitionRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]interface{}) error) error {
	r.streamed = append(r.streamed, table)
	for start := 0; start < len(r.rows[table]); start += batch {
		end := min(start+batch, len(r.rows[table]))
		if e := consume([]string{"Id"}, []string{"BIGINT"}, r.rows[table][start:end]); e != nil {
			return e
		}
		if e := ctx.Err(); e != nil {
			return e
		}
	}
	return nil
}
func (r *partitionFlowReader) ReadPage(ctx context.Context, table string, keys []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	r.pageReads[table]++
	return r.mockReader.ReadPage(ctx, table, keys, offset, limit)
}
func (r *partitionFlowReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return r.primary, nil
}
func (r *partitionFlowReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return r.indexes, nil
}
func (r *partitionFlowReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return r.fks, nil
}
func (r *partitionFlowReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}
func (r *partitionFlowReader) CountPartitionRows(_ context.Context, table string, _ *source.PartitionSpec) (map[string]int64, error) {
	return partitionFlowCounts(r.rows[table], "Src"), nil
}

type partitionFlowWriter struct {
	*mockWriter
	mu           sync.Mutex
	profile      partition.Profile
	actual       func(string) *source.PartitionSpec
	counts       map[string]map[string]int64
	copyErr      error
	createErr    error
	keyColumnErr error
}

func (w *partitionFlowWriter) InspectPartitionProfile(context.Context) (partition.Profile, error) {
	return w.profile, nil
}
func (w *partitionFlowWriter) InspectPartitionTable(_ context.Context, table string) (*source.PartitionSpec, error) {
	return source.ClonePartitionSpec(w.actual(table)), nil
}
func (w *partitionFlowWriter) ValidatePartitionNames(context.Context, string, *source.PartitionSpec) error {
	return nil
}
func (w *partitionFlowWriter) VerifyPartitionKeyColumns(context.Context, string, *partition.Plan) error {
	return w.keyColumnErr
}
func (w *partitionFlowWriter) CountPartitionRows(_ context.Context, table string, _ *source.PartitionSpec) (map[string]int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int64{}
	for key, count := range w.counts[table] {
		out[key] = count
	}
	return out, nil
}
func (w *partitionFlowWriter) CountRows(_ context.Context, table string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var count int64
	for _, n := range w.counts[table] {
		count += n
	}
	return count, nil
}
func (w *partitionFlowWriter) CreateTable(ctx context.Context, stmts []dialect.Statement) error {
	if w.createErr != nil {
		return w.createErr
	}
	return w.mockWriter.CreateTable(ctx, stmts)
}
func (w *partitionFlowWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.copyErr != nil {
		return w.copyErr
	}
	if e := w.mockWriter.CopyData(ctx, table, cols, types, rows); e != nil {
		return e
	}
	if w.counts[table] == nil {
		w.counts[table] = map[string]int64{"Dst.Low": 0, "Dst.High": 0}
	}
	for key, count := range partitionFlowCounts(rows, "Dst") {
		w.counts[table][key] += count
	}
	return nil
}

func partitionFlowCounts(rows [][]interface{}, schema string) map[string]int64 {
	counts := map[string]int64{schema + ".Low": 0, schema + ".High": 0}
	for _, row := range rows {
		n := 0
		switch v := row[0].(type) {
		case int:
			n = v
		case int64:
			n = int(v)
		}
		leaf := "Low"
		if n >= 10 {
			leaf = "High"
		}
		counts[schema+"."+leaf]++
	}
	return counts
}
func partitionFlowSpec() *source.PartitionSpec {
	return &source.PartitionSpec{Method: "RANGE COLUMNS", Keys: []source.PartitionKey{{Column: "Id", DataType: "bigint"}}, Partitions: []source.Partition{{Schema: "Src", Name: "Low", Ordinal: 1, Lower: []source.PartitionValue{{Kind: "minvalue"}}, Upper: []source.PartitionValue{{Kind: "number", Text: "10"}}, LowerInclusive: true}, {Schema: "Src", Name: "High", Ordinal: 2, Lower: []source.PartitionValue{{Kind: "number", Text: "10"}}, Upper: []source.PartitionValue{{Kind: "maxvalue"}}, LowerInclusive: true}}}
}

func newPartitionFlow(t *testing.T, names ...string) (*Migrator, *partitionFlowReader, *partitionFlowWriter) {
	t.Helper()
	r := &partitionFlowReader{mockReader: &mockReader{tables: names, ddl: map[string]*source.TableDDLInfo{}, pk: map[string][]string{}, rows: map[string][][]interface{}{}}, specs: map[string]*source.PartitionSpec{}, roots: map[string]string{}, metadataErrors: map[string]error{}, metadataCalls: map[string]int{}, pageReads: map[string]int{}}
	for _, name := range names {
		r.ddl[name] = &source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "Id", DataType: "bigint", IsNullable: false}}}
		r.pk[name] = []string{"Id"}
		r.rows[name] = [][]interface{}{{1}, {11}}
	}
	w := &partitionFlowWriter{mockWriter: &mockWriter{}, counts: map[string]map[string]int64{}, profile: partition.Profile{Product: "postgres", Version: "16.6", Features: map[string]bool{"postgres_declarative": true, partition.FeatureRange: true, partition.FeatureList: true, partition.FeatureHash: true, partition.FeatureExpressions: true, partition.FeatureSubpartition: true, partition.FeatureMultiRange: true}}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, Config{Mode: "all", Content: "both", TargetSchema: "Dst", TargetDBType: "postgres", PageSize: 1, MaxParallel: 1, IntraTableParallel: 4})
	w.actual = func(table string) *source.PartitionSpec {
		if m.partitionState == nil || m.partitionState.tables[table] == nil || m.partitionState.tables[table].Plan == nil {
			return nil
		}
		return m.partitionState.tables[table].Plan.Target
	}
	return m, r, w
}

func TestPartitionsPartialPreflightContinuesIndependentTable(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Blocked", "Good")
	r.specs["Blocked"] = &source.PartitionSpec{Unsupported: "native shape unavailable"}
	p := m.PreflightPartitions(context.Background())
	require.False(t, p.Ready)
	require.True(t, p.CanStart)
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Len(t, w.created, 1)
	require.Contains(t, w.created[0], `"Good"`)
	require.NotContains(t, strings.Join(w.created, "\n"), `"Blocked"`)
	require.Contains(t, w.copied, "Good")
	require.NotContains(t, w.copied, "Blocked")
}

func TestPartitionsStableRootStreamAndPhysicalChildDedup(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root", "PhysicalLow", "PhysicalHigh")
	r.specs["Root"] = partitionFlowSpec()
	r.roots = map[string]string{"PhysicalLow": "Root", "PhysicalHigh": "Root", "Src.PhysicalLow": "Root"}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, []string{"Root"}, r.streamed)
	require.Zero(t, r.pageReads["Root"])
	require.Len(t, w.created, 1)
	require.NotContains(t, strings.Join(w.created, "\n"), "PhysicalLow")
	require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
	require.Equal(t, map[string]int64{"Src.Low": 1, "Src.High": 1}, report.PartitionMigration.Tables[0].SourceRows)
}

func TestPartitionsMetadataFailureAndAllBlockedDoNotWrite(t *testing.T) {
	for _, cause := range []string{"catalog error", "unsupported"} {
		t.Run(cause, func(t *testing.T) {
			m, r, w := newPartitionFlow(t, "Root")
			if cause == "catalog error" {
				r.metadataErrors["Root"] = errors.New("catalog denied")
			} else {
				r.specs["Root"] = &source.PartitionSpec{Unsupported: "not supported"}
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.False(t, report.PartitionMigration.CanStart)
			require.Empty(t, w.created)
			require.Empty(t, w.copied)
		})
	}
}

func TestPartitionsCDCDoesNotEnablePartitionWrites(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	m.cfg.MigrationPurpose = "cdc"
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Contains(t, report.PartitionMigration.Tables[0].Error, "CDC")
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestPartitionsDataOnlyMismatchDoesNotWrite(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	m.cfg.Content = "data_only"
	w.actual = func(string) *source.PartitionSpec { return nil }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestPartitionsDataOnlyCountsAppendRelativeToFrozenBaseline(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	m.cfg.Content = "data_only"
	w.counts["Root"] = map[string]int64{"Dst.Low": 5, "Dst.High": 3}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report.PartitionMigration)
	require.Empty(t, w.created)
	result := report.PartitionMigration.Tables[0]
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, map[string]int64{"Dst.Low": 5, "Dst.High": 3}, result.Baseline)
	require.Equal(t, map[string]int64{"Dst.Low": 6, "Dst.High": 4}, result.TargetRows)
}

func TestPartitionsSourceDriftBeforeDDLDoesNotDrop(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	r.metadataAt = func(table string, call int) *source.PartitionSpec {
		spec := source.ClonePartitionSpec(r.specs[table])
		if call > 1 {
			spec.Partitions[0].Upper[0].Text = "9"
		}
		return spec
	}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestPartitionsUniqueConstraintCannotGainPartitionKey(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	r.primary = []source.IndexInfo{{TableName: "Root", IndexName: "PK", Columns: []string{"Other"}, IsUnique: true, IsPrimary: true}}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Contains(t, report.PartitionMigration.Tables[0].Error, "未包含分区键")
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestPartitionsRepairManifestPersistsFrozenPlan(t *testing.T) {
	m, r, _ := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	m.cfg.CaptureRepair = true
	m.cfg.Content = "schema_only"
	var saved RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error { saved = manifest; return nil }
	report := m.Run(context.Background())
	require.NotNil(t, report.RepairManifest)
	require.NotEmpty(t, saved.Objects)
	found := false
	for _, object := range saved.Objects {
		if object.Kind == "table" && object.SourceTable == "Root" {
			found = true
			require.NotNil(t, object.PartitionPlan)
			require.NotEmpty(t, object.PartitionPlan.Fingerprint)
			require.Len(t, object.PartitionPlan.Mapping, 2)
		}
	}
	require.True(t, found)
}

func TestPartitionsPriorTargetCannotHideCreateOrCopyFailure(t *testing.T) {
	for _, phase := range []string{"create", "copy"} {
		t.Run(phase, func(t *testing.T) {
			m, r, w := newPartitionFlow(t, "Root")
			r.specs["Root"] = partitionFlowSpec()
			w.counts["Root"] = map[string]int64{"Dst.Low": 1, "Dst.High": 1}
			if phase == "create" {
				w.createErr = errors.New("create failed")
			} else {
				w.copyErr = errors.New("copy failed")
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.NotEqual(t, "succeeded", report.PartitionMigration.Tables[0].Status)
		})
	}
}

func TestPartitionsPhysicalChildSelectionDoesNotBroadenToRoot(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root", "PhysicalLow")
	r.specs["Root"] = partitionFlowSpec()
	r.roots["PhysicalLow"] = "Root"
	m.cfg.Mode, m.cfg.Filter = "include", "PhysicalLow"
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Contains(t, report.PartitionMigration.Tables[0].Error, "请选择逻辑根表 Root")
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestPartitionsUnreliableRootInventoryStopsBeforeWrites(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Ordinary")
	r.rootError = errors.New("parent catalog denied")
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Contains(t, report.PartitionMigration.FatalError, "parent catalog denied")
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
}

func TestPartitionsLeafRelationNameCollisionBlocksBothOwners(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root", "Low", "Good")
	r.specs["Root"] = partitionFlowSpec()
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.True(t, report.PartitionMigration.CanStart)
	require.Len(t, w.created, 1)
	require.Contains(t, w.created[0], `"Good"`)
	require.NotContains(t, strings.Join(w.created, "\n"), `"Root"`)
	require.NotContains(t, strings.Join(w.created, "\n"), `"Low"`)
}

type partitionLegacyWriter struct{ target.Writer }

func TestPartitionsLegacyReaderDoesNotCallOptionalWriterCapabilities(t *testing.T) {
	m := NewMigrator(&mockReader{}, &partitionLegacyWriter{}, &Job{LogCh: make(chan string, 10)}, Config{TargetDBType: "postgres"})
	var preflight *PartitionPreflight
	require.NotPanics(t, func() { preflight = m.PreflightPartitions(context.Background()) })
	require.True(t, preflight.Ready)
	require.True(t, preflight.CanStart)
	require.Empty(t, preflight.Tables)
}

type partitionCancelCountWriter struct {
	*partitionFlowWriter
	cancel context.CancelFunc
}

func (w *partitionCancelCountWriter) CountPartitionRows(ctx context.Context, table string, spec *source.PartitionSpec) (map[string]int64, error) {
	counts, err := w.partitionFlowWriter.CountPartitionRows(ctx, table, spec)
	w.cancel()
	return counts, err
}

func TestPartitionsCancellationAfterFinalLeafCountCannotSucceed(t *testing.T) {
	m, r, w := newPartitionFlow(t, "Root")
	r.specs["Root"] = partitionFlowSpec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.writer = &partitionCancelCountWriter{partitionFlowWriter: w, cancel: cancel}
	report := m.Run(ctx)
	require.Equal(t, "incomplete", report.PartitionMigration.Tables[0].Status)
	require.Contains(t, report.PartitionMigration.Tables[0].Error, "canceled")
	require.True(t, report.HasFailures())
}

func TestPartitionsDataOnlyRequiresEveryBaselineLeaf(t *testing.T) {
	for _, counts := range []map[string]int64{
		{"Dst.Low": 1},
		{"Dst.Low": 1, "Dst.High": -1},
		{"Dst.Low": 1, "0": 1}, // zero is not a frozen native ordinal fallback
	} {
		m, r, w := newPartitionFlow(t, "Root")
		r.specs["Root"] = partitionFlowSpec()
		m.cfg.Content = "data_only"
		w.counts["Root"] = counts
		report := m.Run(context.Background())
		require.True(t, report.HasFailures())
		require.Equal(t, "blocked", report.PartitionMigration.Tables[0].Status)
		require.Contains(t, report.PartitionMigration.Tables[0].Error, "追加基线")
		require.Empty(t, w.copied)
		require.Empty(t, r.streamed)
	}
}
