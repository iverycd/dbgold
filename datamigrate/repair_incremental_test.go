package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func cloneIncrementalRepairValue[T any](t *testing.T, value T) T {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	var result T
	require.NoError(t, json.Unmarshal(b, &result))
	return result
}

type incrementalRepairStore struct {
	t           *testing.T
	fullCalls   int
	objectCalls int
	objects     map[string]repair.Object
}

func (s *incrementalRepairStore) saveManifest(manifest RepairManifest) error {
	s.fullCalls++
	// Callbacks must never escape in exported configuration snapshots.
	require.Nil(s.t, manifest.Config.RepairSink)
	require.Nil(s.t, manifest.Config.RepairObjectSink)
	s.objects = map[string]repair.Object{}
	for _, object := range cloneIncrementalRepairValue(s.t, manifest).Objects {
		s.objects[object.ID] = object
	}
	return nil
}

func (s *incrementalRepairStore) saveObject(object repair.Object) error {
	s.objectCalls++
	if s.objects == nil {
		s.objects = map[string]repair.Object{}
	}
	s.objects[object.ID] = cloneIncrementalRepairValue(s.t, object)
	return nil
}

func incrementalRepairReader(names ...string) *mockReader {
	r := &mockReader{tables: names, ddl: map[string]*source.TableDDLInfo{}}
	for _, name := range names {
		r.ddl[name] = &source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}}}
	}
	return r
}

func observeIncrementalRepairSQL(ctx context.Context, statements []dialect.Statement, executed *int) error {
	for _, statement := range statements {
		if err := repair.Observe(ctx, statement.SQL, "running", nil); err != nil {
			return err
		}
		if executed != nil {
			*executed++
		}
		if err := repair.Observe(ctx, statement.SQL, "succeeded", nil); err != nil {
			return err
		}
	}
	return nil
}

func TestRepairIncrementalPersistenceScalesWithTableExecutions(t *testing.T) {
	names := make([]string, 118)
	for i := range names {
		names[i] = fmt.Sprintf("Table_%03d", i)
	}
	r := &repairIndexReader{mockReader: incrementalRepairReader(names...)}
	// 118 tables + 118 owners + 2468 indexes = 2704 repair objects.
	for i := 0; i < 2468; i++ {
		r.indexes = append(r.indexes, source.IndexInfo{TableName: names[i%len(names)], IndexName: fmt.Sprintf("Index_%04d", i), Columns: []string{"ID"}})
	}
	m, _ := newTestMigrator(r, &mockWriter{})
	m.cfg.CaptureRepair, m.cfg.ChangeOwner, m.cfg.TargetSchema = true, true, "dst"
	saved := &incrementalRepairStore{t: t}
	m.cfg.RepairSink, m.cfg.RepairObjectSink = saved.saveManifest, saved.saveObject
	ctx := context.Background()
	m.beginRepair(ctx)
	require.Len(t, saved.objects, 2704)
	require.Equal(t, 1, saved.fullCalls)
	executed := 0
	for _, name := range names {
		ss, _, err := m.buildCreateTableDDL(ctx, name)
		require.NoError(t, err)
		require.Len(t, ss, 2)
		err = m.captureRepairCall(ctx, repair.Object{Kind: "table", Name: name, Table: name, TargetName: name}, ss, func(ctx context.Context) error {
			return observeIncrementalRepairSQL(ctx, ss, &executed)
		})
		require.NoError(t, err)
		ownerSQL := m.repairOwnerStatements("TABLE", name, "dst")
		require.NoError(t, m.captureRepairCall(ctx, repair.Object{Kind: "owner", Name: name, Table: name, TargetName: "TABLE:" + name}, ownerSQL, func(ctx context.Context) error {
			return observeIncrementalRepairSQL(ctx, ownerSQL, &executed)
		}))
	}
	require.Equal(t, 354, executed)
	require.Equal(t, 1180, saved.objectCalls, "each table persists six transitions and each owner four")
	require.Equal(t, 1, saved.fullCalls, "individual execution transitions must not rewrite the manifest")
	report := newMigrationReport()
	m.finishRepair(ctx, &report)
	require.Equal(t, 2, saved.fullCalls)
	require.Equal(t, 1180, saved.objectCalls)
	require.Len(t, report.RepairManifest.Objects, 2704)
	for _, object := range report.RepairManifest.Objects {
		require.Equal(t, cloneIncrementalRepairValue(t, object), saved.objects[object.ID])
	}
}

func TestRepairIncrementalAndLegacySinksProduceIdenticalManifest(t *testing.T) {
	run := func(incremental bool) RepairManifest {
		r := &repairIndexReader{mockReader: incrementalRepairReader("T"), indexes: []source.IndexInfo{{TableName: "T", IndexName: "idx_expression", Unsupported: "unsupported expression"}}}
		w := newRepairIndexWriter()
		m, _ := newTestMigrator(r, w)
		m.cfg.CaptureRepair, m.cfg.TargetSchema = true, "dst"
		saved := &incrementalRepairStore{t: t}
		m.cfg.RepairSink = saved.saveManifest
		if incremental {
			m.cfg.RepairObjectSink = saved.saveObject
		}
		ctx := context.Background()
		m.beginRepair(ctx)
		ss, _, err := m.buildCreateTableDDL(ctx, "T")
		require.NoError(t, err)
		require.NoError(t, m.captureRepairCall(ctx, repair.Object{Kind: "table", Name: "T", Table: "T", TargetName: "T"}, ss, func(ctx context.Context) error {
			return observeIncrementalRepairSQL(ctx, ss, nil)
		}))

		// Dynamic work is absent from the initial manifest. Repeated physical SQL
		// still identifies separate units when a transaction rolls back.
		repeated := []dialect.Statement{{SQL: "ALTER TABLE T ADD extra INT"}, {SQL: "ALTER TABLE T ADD extra INT"}}
		duplicate := errors.New("duplicate column")
		err = m.captureRepairCall(ctx, repair.Object{Kind: "index", Name: "idx_dynamic", Table: "T", TargetName: "idx_dynamic"}, repeated, func(ctx context.Context) error {
			for _, statement := range repeated {
				if err := repair.Observe(ctx, statement.SQL, "running", nil); err != nil {
					return err
				}
			}
			if err := repair.Observe(ctx, repeated[1].SQL, "failed", duplicate); err != nil {
				return err
			}
			if err := repair.Observe(ctx, repeated[0].SQL, "rolled_back", nil); err != nil {
				return err
			}
			return duplicate
		})
		require.ErrorIs(t, err, duplicate)
		dynamic := saved.objects[repairID("index", "T", "idx_dynamic")]
		require.Len(t, dynamic.Units, 2)
		require.Equal(t, "rolled_back", dynamic.Units[0].Status)
		require.Equal(t, "failed", dynamic.Units[1].Status)

		require.ErrorContains(t, m.repairIndex(ctx, r.indexes[0]), "unsupported expression")
		require.Empty(t, w.created)
		require.Empty(t, w.dia.generated)
		unsupported := saved.objects[repairID("index", "T", "idx_expression")]
		require.Equal(t, "failed", unsupported.Status)
		require.Empty(t, unsupported.Units)
		require.Empty(t, unsupported.DDL)

		viewSQL := []dialect.Statement{{SQL: "CREATE VIEW dynamic_view AS SELECT ID FROM T"}}
		require.NoError(t, m.captureRepairCall(ctx, repair.Object{Kind: "view", Name: "dynamic_view", TargetName: "dynamic_view"}, viewSQL, func(ctx context.Context) error {
			return observeIncrementalRepairSQL(ctx, viewSQL, nil)
		}))
		require.NoError(t, m.repairViewDependencies("dynamic_view", []string{"T"}))
		require.Equal(t, []string{repairID("table", "T", "T")}, saved.objects[repairID("view", "", "dynamic_view")].Dependencies)

		report := newMigrationReport()
		report.RowCounts = []TableRowCount{{Table: "T", Match: true}}
		m.finishRepair(ctx, &report)
		require.Len(t, report.RepairManifest.Objects, 4)
		for _, object := range report.RepairManifest.Objects {
			require.Equal(t, cloneIncrementalRepairValue(t, object), saved.objects[object.ID])
		}
		if incremental {
			require.Equal(t, 2, saved.fullCalls)
			require.Greater(t, saved.objectCalls, 0)
		} else {
			require.Greater(t, saved.fullCalls, 2, "legacy callback remains the fallback for all transitions")
			require.Zero(t, saved.objectCalls)
		}
		return cloneIncrementalRepairValue(t, *report.RepairManifest)
	}
	require.Equal(t, run(false), run(true))
}

func TestRepairIncrementalSinkFailuresRemainSynchronous(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fullFailAt   int
		objectFailAt int
		wantSQL      int
	}{
		{name: "initial manifest", fullFailAt: 1},
		{name: "object start", objectFailAt: 1},
		{name: "before SQL", objectFailAt: 2},
		{name: "after SQL", objectFailAt: 3, wantSQL: 1},
		{name: "object completion", objectFailAt: 4, wantSQL: 1},
		{name: "final manifest", fullFailAt: 2, wantSQL: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMigrator(incrementalRepairReader("T"), &mockWriter{})
			failure := errors.New("repair storage unavailable")
			fullCalls, objectCalls, executed := 0, 0, 0
			m.cfg.RepairSink = func(RepairManifest) error {
				fullCalls++
				if fullCalls == tc.fullFailAt {
					return failure
				}
				return nil
			}
			m.cfg.RepairObjectSink = func(repair.Object) error {
				objectCalls++
				if objectCalls == tc.objectFailAt {
					return failure
				}
				return nil
			}
			ctx := context.Background()
			m.beginRepair(ctx)
			ss := []dialect.Statement{{SQL: "CREATE TABLE T (ID INT)"}}
			err := m.captureRepairCall(ctx, repair.Object{Kind: "table", Name: "T", Table: "T", TargetName: "T"}, ss, func(ctx context.Context) error {
				return observeIncrementalRepairSQL(ctx, ss, &executed)
			})
			report := newMigrationReport()
			if tc.fullFailAt == 2 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, failure)
				report.Tables = CategoryReport{Total: 1, Failed: 1, Items: []ObjectResult{{Name: "T", Error: err.Error()}}}
				// A later operation cannot silently resume after a failed durable write.
				require.ErrorIs(t, m.captureRepairCall(ctx, repair.Object{Kind: "table", Name: "Next", Table: "Next", TargetName: "Next"}, ss, func(ctx context.Context) error {
					executed++
					return nil
				}), failure)
			}
			require.Equal(t, tc.wantSQL, executed)
			m.finishRepair(ctx, &report)
			require.True(t, report.HasFailures())
			if tc.fullFailAt == 2 {
				require.Len(t, report.Data.Items, 1)
				require.Equal(t, "修复记录持久化", report.Data.Items[0].Name)
				require.Contains(t, report.Data.Items[0].Error, failure.Error())
			}
		})
	}
}

func TestRepairIncrementalViewDependenciesPersistImmediatelyAndStopOnFailure(t *testing.T) {
	m, _ := newTestMigrator(incrementalRepairReader("T", "Other"), &mockWriter{})
	saved := &incrementalRepairStore{t: t}
	m.cfg.RepairSink, m.cfg.RepairObjectSink = saved.saveManifest, saved.saveObject
	ctx := context.Background()
	m.beginRepair(ctx)
	ss := []dialect.Statement{{SQL: "CREATE VIEW dynamic_view AS SELECT ID FROM T"}}
	view := repair.Object{Kind: "view", Name: "dynamic_view", TargetName: "dynamic_view"}
	require.NoError(t, m.captureRepairCall(ctx, view, ss, func(ctx context.Context) error {
		return observeIncrementalRepairSQL(ctx, ss, nil)
	}))
	viewID := repairID("view", "", "dynamic_view")
	require.Empty(t, saved.objects[viewID].Dependencies)
	before := saved.objectCalls
	require.NoError(t, m.repairViewDependencies("dynamic_view", []string{"T"}))
	require.Equal(t, before+1, saved.objectCalls)
	require.Equal(t, []string{repairID("table", "T", "T")}, saved.objects[viewID].Dependencies)
	require.NoError(t, m.repairViewDependencies("dynamic_view", []string{"T"}))
	require.Equal(t, before+1, saved.objectCalls, "unchanged dependencies do not need another write")
	require.Equal(t, 1, saved.fullCalls)

	failure := errors.New("dependency storage unavailable")
	m.cfg.RepairObjectSink = func(repair.Object) error { return failure }
	require.ErrorIs(t, m.repairViewDependencies("dynamic_view", []string{"Other"}), failure)
	called := false
	require.ErrorIs(t, m.captureRepairCall(ctx, view, ss, func(context.Context) error {
		called = true
		return nil
	}), failure)
	require.False(t, called)
	require.Equal(t, []string{repairID("table", "T", "T")}, saved.objects[viewID].Dependencies)
}

func TestRepairIncrementalFailureAllowsRollbackButBlocksNewSQLAndSurvivesFinalSave(t *testing.T) {
	m, _ := newTestMigrator(incrementalRepairReader("T"), &mockWriter{})
	saved := &incrementalRepairStore{t: t}
	m.cfg.RepairSink = saved.saveManifest
	failure := errors.New("terminal event could not be persisted")
	calls := 0
	m.cfg.RepairObjectSink = func(object repair.Object) error {
		calls++
		if calls == 4 { // object start, two running events, then failure event
			return failure
		}
		return saved.saveObject(object)
	}
	ctx := context.Background()
	m.beginRepair(ctx)
	ss := []dialect.Statement{{SQL: "ALTER TABLE T ADD extra INT"}, {SQL: "ALTER TABLE T ADD extra INT"}}
	err := m.captureRepairCall(ctx, repair.Object{Kind: "table", Name: "T", Table: "T", TargetName: "T"}, ss, func(ctx context.Context) error {
		for _, statement := range ss {
			require.NoError(t, repair.Observe(ctx, statement.SQL, "running", nil))
		}
		require.ErrorIs(t, repair.Observe(ctx, ss[1].SQL, "failed", errors.New("duplicate column")), failure)
		// A storage outage must not prevent recording rollback of SQL already run.
		require.NoError(t, repair.Observe(ctx, ss[0].SQL, "rolled_back", nil))
		before := calls
		require.ErrorIs(t, repair.Observe(ctx, "ALTER TABLE T ADD next INT", "running", nil), failure)
		require.Equal(t, before, calls)
		// Even a writer that ignores the observer error cannot report success.
		return nil
	})
	require.ErrorIs(t, err, failure)
	object := saved.objects[repairID("table", "T", "T")]
	require.Len(t, object.Units, 2)
	require.Equal(t, "rolled_back", object.Units[0].Status)
	require.Equal(t, "failed", object.Units[1].Status)
	require.Equal(t, "failed", object.Status)

	// The caller can treat an owner/distribution error as a warning; finish still
	// includes the durable-write failure even though the final full save succeeds.
	report := newMigrationReport()
	m.finishRepair(ctx, &report)
	require.Equal(t, 2, saved.fullCalls)
	require.True(t, report.HasFailures())
	require.Len(t, report.Data.Items, 1)
	require.Equal(t, "修复记录持久化", report.Data.Items[0].Name)
	require.Contains(t, report.Data.Items[0].Error, failure.Error())
}

func TestRepairIncrementalSinkRequiresInitialManifestCallback(t *testing.T) {
	w := &mockWriter{}
	m, _ := newTestMigrator(incrementalRepairReader("T"), w)
	m.cfg.CaptureRepair = true
	objectCalls := 0
	m.cfg.RepairObjectSink = func(repair.Object) error {
		objectCalls++
		return nil
	}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
	require.Zero(t, objectCalls)
	require.ErrorContains(t, m.repairs.sinkErr, "完整清单")
}

type incrementalRepairTransientReader struct {
	*mockReader
	listCalls int
}

func (r *incrementalRepairTransientReader) ListTables(ctx context.Context) ([]string, error) {
	r.listCalls++
	if r.listCalls == 1 {
		return nil, errors.New("temporary source metadata read failure")
	}
	return r.mockReader.ListTables(ctx)
}

func TestRepairIncrementalPreparationReadFailureStopsRunAndAllowsExplicitRetry(t *testing.T) {
	r := &incrementalRepairTransientReader{mockReader: incrementalRepairReader("T")}
	w := &mockWriter{}
	m, _ := newTestMigrator(r, w)
	m.cfg.CaptureRepair, m.cfg.Content = true, "schema_only"
	saved := &incrementalRepairStore{t: t}
	m.cfg.RepairSink, m.cfg.RepairObjectSink = saved.saveManifest, saved.saveObject

	first := m.Run(context.Background())
	require.True(t, first.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.copied)
	require.Equal(t, 1, r.listCalls, "preparation failure must stop this run before a second metadata read")
	require.ErrorContains(t, m.repairs.sinkErr, "temporary source metadata read failure")

	second := m.Run(context.Background())
	require.False(t, second.HasFailures())
	require.Nil(t, m.repairs.sinkErr)
	require.Len(t, w.created, 1)
	require.Equal(t, 1, second.Tables.Success)
	require.Greater(t, saved.objectCalls, 0)
}
