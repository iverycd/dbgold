package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

type nativeIntervalFlow struct {
	mu        sync.Mutex
	persisted bool
	prepared  bool
	calls     int
	events    []string
}

func (f *nativeIntervalFlow) event(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, name)
}

type nativeIntervalFlowReader struct {
	*nativeTestReader
	flow *nativeIntervalFlow
}

func (r *nativeIntervalFlowReader) OpenDamengNativeScan(ctx context.Context, table damengnative.Table) (damengnative.Scan, error) {
	r.flow.mu.Lock()
	ready := r.flow.persisted && r.flow.prepared
	r.flow.events = append(r.flow.events, "scan")
	r.flow.mu.Unlock()
	if !ready {
		return nil, fmt.Errorf("source scan started before durable interval preparation")
	}
	return r.nativeTestReader.OpenDamengNativeScan(ctx, table)
}

type nativeIntervalFlowWriter struct {
	*nativeTestWriter
	flow      *nativeIntervalFlow
	onPrepare func(context.Context) error
}

func (w *nativeIntervalFlowWriter) ExecuteDamengNative(ctx context.Context, statement string) error {
	w.flow.event("ddl")
	return w.nativeTestWriter.ExecuteDamengNative(ctx, statement)
}

func (w *nativeIntervalFlowWriter) RestoreDamengNativeIntervalPartitions(ctx context.Context, object damengnative.Object, unit damengnative.Unit) error {
	w.flow.mu.Lock()
	w.flow.calls++
	w.flow.events = append(w.flow.events, "interval")
	persisted := w.flow.persisted
	w.flow.mu.Unlock()
	if !persisted || len(w.statements) == 0 || object.Interval == nil || unit.SQL != damengnative.IntervalPartitionSQL(object.Schema, object.Name, object.Interval) {
		return fmt.Errorf("interval operation lacks its durable scope-bound recipe or base table")
	}
	if err := repair.Observe(ctx, unit.SQL, "running", nil); err != nil {
		return err
	}
	var err error
	if w.onPrepare != nil {
		err = w.onPrepare(ctx)
	}
	if err != nil {
		status := "failed"
		if ctx.Err() != nil {
			status = "unknown"
		}
		return errors.Join(err, repair.Observe(ctx, unit.SQL, status, err))
	}
	w.flow.mu.Lock()
	w.flow.prepared = true
	w.flow.mu.Unlock()
	return repair.Observe(ctx, unit.SQL, "succeeded", nil)
}

func (w *nativeIntervalFlowWriter) CopyDamengNativeBatch(ctx context.Context, table damengnative.Table, rows [][]any) error {
	w.flow.mu.Lock()
	ready := w.flow.persisted && w.flow.prepared
	w.flow.events = append(w.flow.events, "copy")
	w.flow.mu.Unlock()
	if !ready {
		return fmt.Errorf("data copy started before durable interval preparation")
	}
	return w.nativeTestWriter.CopyDamengNativeBatch(ctx, table, rows)
}

func nativeIntervalFixture(t *testing.T) (*Migrator, *nativeTestReader, *nativeTestWriter, *nativeIntervalFlow) {
	t.Helper()
	m, reader, writer := nativeTestMigrator(Config{Mode: "all", Content: "both", CaptureRepair: true, MaxParallel: 1})
	ddl := `CREATE TABLE "Src"."Mixed"("Value" INT) PARTITION BY RANGE("Value") INTERVAL(1)(PARTITION "Initial" VALUES LESS THAN(1))`
	plan := &damengnative.IntervalPartitionPlan{
		Column: "Value", DefinitionSQL: ddl,
		EmptyTouch: &damengnative.MaterializedEmptyTouch{Table: "Mixed", DefinitionSQL: ddl, Columns: []damengnative.MaterializedEmptyColumn{{Name: "Value", ValueSQL: "0"}}},
		Partitions: []damengnative.IntervalPartition{{Name: "Interval_Exact", HighSQL: "4", SeedSQL: "3", Tablespace: "MAIN", DefinitionSQL: `CREATE TABLE "Src"."Mixed_Interval_Exact"("Value" INT)`}},
	}
	reader.snapshot.Objects[0].Interval = plan
	reader.snapshot.Objects[0].Units = []damengnative.Unit{{SQL: ddl, Phase: "before_data"}, {SQL: damengnative.IntervalPartitionSQL("Src", "Mixed", plan), Phase: "interval_prepare"}}
	reader.snapshot.Tables[0].Columns = []damengnative.Column{{Name: "Value", DataType: "INT", SQLType: "INT", Writable: true}}
	reader.rows = [][]any{{int64(1)}, {int64(3)}}
	flow := &nativeIntervalFlow{}
	m.reader = &nativeIntervalFlowReader{nativeTestReader: reader, flow: flow}
	m.writer = &nativeIntervalFlowWriter{nativeTestWriter: writer, flow: flow}
	return m, reader, writer, flow
}

func TestDamengNativeIntervalPlanPersistsMappedRecipeBeforePreparationAndScan(t *testing.T) {
	m, reader, writer, flow := nativeIntervalFixture(t)
	writer.snapshot.Profile.Schema = `Target"Case`
	original := damengnative.Fingerprint(reader.snapshot.Objects[0])
	expectedPlan, err := damengnative.RewriteIntervalPartitions(reader.snapshot.Objects[0].Interval, "Src", writer.snapshot.Profile.Schema)
	require.NoError(t, err)
	var persisted RepairManifest
	m.cfg.RepairSink = func(manifest RepairManifest) error {
		// Test the durable JSON representation, rather than retaining a pointer
		// that a later preparation phase could accidentally mutate.
		encoded, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &persisted); err != nil {
			return err
		}
		if len(writer.statements) == 0 {
			require.Len(t, persisted.Objects, 1)
			object := persisted.Objects[0]
			require.True(t, object.TrustedNative())
			require.NotNil(t, object.Native.Interval)
			require.Equal(t, damengnative.Fingerprint(expectedPlan), damengnative.Fingerprint(object.Native.Interval), "the complete typed recipe must survive the first durable checkpoint")
			require.Equal(t, `Target"Case`, object.Native.Schema)
			require.Contains(t, object.Native.Interval.DefinitionSQL, `"Target""Case"."Mixed"`)
			require.Equal(t, "Interval_Exact", object.Native.Interval.Partitions[0].Name)
			require.Equal(t, damengnative.IntervalPartitionSQL(object.Native.Schema, object.Native.Name, object.Native.Interval), object.Units[1].SQL)
			flow.mu.Lock()
			flow.persisted = true
			flow.events = append(flow.events, "persist")
			flow.mu.Unlock()
		}
		return nil
	}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report.NativeSchema.Verification)
	require.Equal(t, 1, flow.calls)
	require.Equal(t, 1, reader.scans)
	require.Equal(t, reader.rows, writer.copied)
	require.Equal(t, original, damengnative.Fingerprint(reader.snapshot.Objects[0]), "schema mapping must not mutate the frozen source recipe")
	require.Equal(t, "succeeded", persisted.Objects[0].Units[1].Status)
	positions := map[string]int{}
	for i, event := range flow.events {
		if _, exists := positions[event]; !exists {
			positions[event] = i
		}
	}
	require.Less(t, positions["persist"], positions["ddl"])
	require.Less(t, positions["ddl"], positions["interval"])
	require.Less(t, positions["interval"], positions["scan"])
	require.Less(t, positions["scan"], positions["copy"])
	for _, statement := range writer.statements {
		require.NotContains(t, statement, "DBGOLD_INTERVAL", "the marker must dispatch to the typed executor, never generic SQL")
	}
}

func TestDamengNativeIntervalPreparationFailureOrCancellationDoesNotScan(t *testing.T) {
	for _, variant := range []string{"persist_failure", "prepare_failure", "cancel_prepare"} {
		t.Run(variant, func(t *testing.T) {
			m, reader, writer, flow := nativeIntervalFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var persisted RepairManifest
			m.cfg.RepairSink = func(manifest RepairManifest) error {
				if variant == "persist_failure" {
					return errors.New("durable plan unavailable")
				}
				persisted = manifest
				flow.mu.Lock()
				flow.persisted = true
				flow.mu.Unlock()
				return nil
			}
			m.writer.(*nativeIntervalFlowWriter).onPrepare = func(context.Context) error {
				if variant == "cancel_prepare" {
					cancel()
					return ctx.Err()
				}
				return errors.New("partition preparation failed")
			}
			report := m.Run(ctx)
			require.True(t, report.HasFailures())
			require.Zero(t, reader.scans)
			require.Empty(t, writer.copied)
			require.False(t, flow.prepared)
			if variant == "persist_failure" {
				require.Zero(t, flow.calls)
				require.Empty(t, writer.statements)
			} else {
				require.Equal(t, 1, flow.calls)
				require.Len(t, writer.statements, 1, "only the base CREATE can precede interval preparation")
				want := "failed"
				if variant == "cancel_prepare" {
					want = "unknown"
				}
				require.Equal(t, want, persisted.Objects[0].Units[1].Status)
			}
		})
	}
}

func TestDamengNativeIntervalCapabilityRemainsOptionalAndPreflightIsReadOnly(t *testing.T) {
	m, reader, writer, _ := nativeIntervalFixture(t)
	// The existing native writer still satisfies the required Writer contract.
	// Only a schema that has a frozen interval recipe needs this optional path.
	m.writer = writer
	preflight := m.PreflightDamengNative(context.Background())
	require.False(t, preflight.Ready)
	found := false
	for _, issue := range preflight.Issues {
		found = found || strings.Contains(issue.Reason, "既有间隔分区恢复能力")
	}
	require.True(t, found)
	require.Zero(t, reader.scans)
	require.Empty(t, writer.statements)
	require.Empty(t, writer.copied)
	reader.snapshot.Objects[0].Interval = nil
	reader.snapshot.Objects[0].Units = reader.snapshot.Objects[0].Units[:1]
	require.True(t, m.PreflightDamengNative(context.Background()).Ready, "ordinary schemas must not require the optional interval executor")
}
