package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type commentOptimizationReader struct {
	mockReader
	sourceType string
}

func (r *commentOptimizationReader) DBType() string { return r.sourceType }

type commentOptimizationWriter struct {
	mockWriter
	targetType string
}

func (w *commentOptimizationWriter) DBType() string { return w.targetType }
func (w *commentOptimizationWriter) Dialect() dialect.Dialect {
	if w.targetType == "dameng" {
		return dialect.NewDaMeng()
	}
	return dialect.NewPostgres(w.targetType)
}

// Keep serialized snapshots: retaining Object.Units would let later in-memory
// changes silently alter the test's supposedly durable record.
type commentOptimizationStore struct {
	t         *testing.T
	attempts  int
	failAt    int
	failure   error
	snapshots []repair.Object
}

func (s *commentOptimizationStore) save(o repair.Object) error {
	s.attempts++
	if s.attempts == s.failAt {
		return s.failure
	}
	s.snapshots = append(s.snapshots, cloneIncrementalRepairValue(s.t, o))
	return nil
}

func (s *commentOptimizationStore) latest() repair.Object {
	return s.snapshots[len(s.snapshots)-1]
}

func newCommentOptimizationMigrator(t *testing.T, src, dst string) (*Migrator, *commentOptimizationStore) {
	t.Helper()
	m, _ := newTestMigrator(
		&commentOptimizationReader{sourceType: src},
		&commentOptimizationWriter{targetType: dst},
	)
	m.cfg.CaptureRepair, m.cfg.Content, m.cfg.TargetSchema = true, "schema_only", "APP"
	m.cfg.Objects = []string{"comments"}
	saved := &commentOptimizationStore{t: t}
	m.cfg.RepairSink = func(RepairManifest) error { return nil }
	m.cfg.RepairObjectSink = saved.save
	m.beginRepair(context.Background())
	return m, saved
}

func commentOptimizationObject(column string) (repair.Object, []dialect.Statement) {
	cm := source.CommentInfo{TableName: "Orders", ColumnName: column, Comment: "原始注释 ' unchanged"}
	name := cm.TableName + "." + cm.ColumnName
	return repair.Object{Kind: "comment", Name: name, Table: cm.TableName, TargetName: name, Comment: &cm}, dialect.NewDaMeng().CommentStatements("APP", cm)
}

func requireCommentRunningSnapshot(t *testing.T, o repair.Object, sql string) {
	t.Helper()
	require.Equal(t, "running", o.Status)
	for _, unit := range o.Units {
		if unit.SQL == sql && unit.Status == "running" {
			return
		}
	}
	t.Fatalf("no durable running execution unit for %q: %+v", sql, o.Units)
}

func TestCommentRepairOptimizationPersistsTwoDurableTransitions(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	o, statements := commentOptimizationObject("CaseSensitiveID")
	sql := statements[0].SQL
	executed := []string{}
	err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
		require.Len(t, saved.snapshots, 1)
		requireCommentRunningSnapshot(t, saved.latest(), sql)
		if err := repair.Observe(ctx, sql, "running", nil); err != nil {
			return err
		}
		require.Len(t, saved.snapshots, 1, "the already durable running unit needs no duplicate save")
		executed = append(executed, sql)
		return repair.Observe(ctx, sql, "succeeded", nil)
	})
	require.NoError(t, err)
	require.Equal(t, []string{sql}, executed)
	require.Equal(t, 2, saved.attempts)
	require.Len(t, saved.snapshots, 2)
	requireCommentRunningSnapshot(t, saved.snapshots[0], sql)
	final := saved.latest()
	require.Equal(t, "succeeded", final.Status)
	require.Equal(t, "succeeded", final.Units[0].Status)
	require.False(t, final.Units[0].Selected)
	require.Equal(t, o.Comment, final.Comment)
	require.Equal(t, sql, final.Units[0].SQL)
}

func TestCommentRepairOptimizationInitialSaveFailurePreventsSQL(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	saved.failAt, saved.failure = 1, errors.New("initial repair write failed")
	o, statements := commentOptimizationObject("ID")
	called := false
	err := m.captureRepairCall(context.Background(), o, statements, func(context.Context) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, saved.failure)
	require.False(t, called)
	require.Empty(t, saved.snapshots)
	require.Len(t, m.repairs.objects, 1)
	require.Equal(t, "pending", m.repairs.objects[0].Units[0].Status, "a rejected initial save cannot leave the SQL marked as started")
}

func TestCommentRepairOptimizationFinalSaveFailureBlocksFollowingSQL(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	saved.failAt, saved.failure = 2, errors.New("terminal repair write failed")
	o, statements := commentOptimizationObject("ID")
	executed := 0
	err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
		return observeIncrementalRepairSQL(ctx, statements, &executed)
	})
	require.ErrorIs(t, err, saved.failure)
	require.Equal(t, 1, executed)
	require.Len(t, saved.snapshots, 1)
	requireCommentRunningSnapshot(t, saved.latest(), statements[0].SQL)
	next, nextSQL := commentOptimizationObject("NextID")
	err = m.captureRepairCall(context.Background(), next, nextSQL, func(context.Context) error {
		executed++
		return nil
	})
	require.ErrorIs(t, err, saved.failure)
	require.Equal(t, 1, executed, "storage failure must stop subsequent target SQL")
	require.Equal(t, 2, saved.attempts)
}

func TestCommentRepairOptimizationPreservesFailuresAndUnknownResults(t *testing.T) {
	for _, tc := range []struct {
		name, event, want string
		err               error
	}{
		{name: "definite database failure", event: "failed", want: "failed", err: errors.New("column does not exist")},
		{name: "canceled execution", event: "unknown", want: "unknown", err: context.Canceled},
		{name: "deadline exceeded", event: "unknown", want: "unknown", err: context.DeadlineExceeded},
		{name: "missing terminal after cancellation", want: "unknown", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
			o, statements := commentOptimizationObject("ID")
			executed := 0
			err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
				if err := repair.Observe(ctx, statements[0].SQL, "running", nil); err != nil {
					return err
				}
				executed++
				if tc.event != "" {
					require.NoError(t, repair.Observe(ctx, statements[0].SQL, tc.event, tc.err))
				}
				return tc.err
			})
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, 1, executed)
			require.Equal(t, tc.want, saved.latest().Status)
			require.Equal(t, tc.want, saved.latest().Units[0].Status)
			require.True(t, saved.latest().Units[0].Selected)
			require.Equal(t, 2, saved.attempts)
		})
	}
}

func TestCommentRepairOptimizationMissingTerminalCannotReportSuccess(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	o, statements := commentOptimizationObject("ID")
	err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
		return repair.Observe(ctx, statements[0].SQL, "running", nil)
	})
	require.Error(t, err)
	require.Equal(t, "unknown", saved.latest().Status)
	require.Equal(t, "unknown", saved.latest().Units[0].Status)
}

func TestCommentRepairOptimizationObservedFailureCannotBeHiddenByNilWriterResult(t *testing.T) {
	for _, status := range []string{"failed", "rolled_back"} {
		t.Run(status, func(t *testing.T) {
			m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
			o, statements := commentOptimizationObject("ID")
			failure := errors.New("target SQL did not apply")
			executed := 0
			err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
				if err := repair.Observe(ctx, statements[0].SQL, "running", nil); err != nil {
					return err
				}
				executed++
				require.NoError(t, repair.Observe(ctx, statements[0].SQL, status, failure))
				// A malformed Writer must not turn observed failure into success.
				return nil
			})
			require.Error(t, err)
			require.Equal(t, 1, executed)
			require.Equal(t, "failed", saved.latest().Status)
			require.NotEmpty(t, saved.latest().Error)
			require.Equal(t, status, saved.latest().Units[0].Status)
			require.True(t, saved.latest().Units[0].Selected)
			require.False(t, saved.latest().AllUnitsSucceeded())
		})
	}
}

func TestCommentRepairOptimizationPreservesUnobservedWriterContract(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	o, statements := commentOptimizationObject("ID")
	calls := 0
	err := m.captureRepairCall(context.Background(), o, statements, func(context.Context) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, 2, saved.attempts)
	require.Equal(t, "succeeded", saved.latest().Status)
	require.True(t, saved.latest().AllUnitsSucceeded())
}

func TestCommentRepairOptimizationInterruptedExecutionLeavesDurableRunning(t *testing.T) {
	for _, terminalObserved := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminalObserved=%t", terminalObserved), func(t *testing.T) {
			m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
			o, statements := commentOptimizationObject("ID")
			crash := errors.New("simulated process interruption")
			executed := 0
			require.PanicsWithValue(t, crash, func() {
				_ = m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
					require.NoError(t, repair.Observe(ctx, statements[0].SQL, "running", nil))
					executed++
					if terminalObserved {
						require.NoError(t, repair.Observe(ctx, statements[0].SQL, "succeeded", nil))
					}
					panic(crash)
				})
			})
			require.Equal(t, 1, executed)
			require.Equal(t, 1, saved.attempts)
			require.Len(t, saved.snapshots, 1)
			requireCommentRunningSnapshot(t, saved.latest(), statements[0].SQL)
			require.False(t, saved.latest().AllUnitsSucceeded(), "the durable snapshot must require reconciliation")
		})
	}
}

func TestCommentRepairOptimizationExtraEventsPersistWithoutReplaying(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	o, statements := commentOptimizationObject("ID")
	_, extra := commentOptimizationObject("ExtraID")
	executed := []string{}
	calls := 0
	err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
		calls++
		for _, sql := range []string{statements[0].SQL, extra[0].SQL} {
			if err := repair.Observe(ctx, sql, "running", nil); err != nil {
				return err
			}
			requireCommentRunningSnapshot(t, saved.latest(), sql)
			executed = append(executed, sql)
			if err := repair.Observe(ctx, sql, "succeeded", nil); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, []string{statements[0].SQL, extra[0].SQL}, executed)
	require.GreaterOrEqual(t, saved.attempts, 4, "unexpected SQL restores per-event persistence")
	require.Len(t, saved.latest().Units, 2)
	require.True(t, saved.latest().AllUnitsSucceeded())
}

func TestCommentRepairOptimizationMismatchedSQLRemainsConservative(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	o, statements := commentOptimizationObject("PlannedID")
	_, actual := commentOptimizationObject("ActualID")
	executed := []string{}
	calls := 0
	err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
		calls++
		if err := repair.Observe(ctx, actual[0].SQL, "running", nil); err != nil {
			return err
		}
		requireCommentRunningSnapshot(t, saved.latest(), actual[0].SQL)
		executed = append(executed, actual[0].SQL)
		return repair.Observe(ctx, actual[0].SQL, "succeeded", nil)
	})
	require.Error(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, []string{actual[0].SQL}, executed)
	require.Equal(t, "unknown", saved.latest().Status)
	for _, unit := range saved.latest().Units {
		if unit.SQL == statements[0].SQL {
			require.NotEqual(t, "succeeded", unit.Status, "the planned SQL was never observed or executed")
		}
		if unit.SQL == actual[0].SQL {
			require.Equal(t, "succeeded", unit.Status)
		}
	}
	require.GreaterOrEqual(t, saved.attempts, 4)
}

func TestCommentRepairOptimizationRepeatedRunningEventsRestorePerEventSaves(t *testing.T) {
	m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
	o, statements := commentOptimizationObject("ID")
	calls, executed := 0, 0
	err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
		calls++
		require.NoError(t, repair.Observe(ctx, statements[0].SQL, "running", nil))
		before := saved.attempts
		require.NoError(t, repair.Observe(ctx, statements[0].SQL, "running", nil))
		require.Equal(t, before+1, saved.attempts, "an unexpected event must immediately restore synchronous saving")
		requireCommentRunningSnapshot(t, saved.latest(), statements[0].SQL)
		executed++
		return repair.Observe(ctx, statements[0].SQL, "succeeded", nil)
	})
	require.Error(t, err, "one terminal event cannot confirm two running execution units")
	require.Equal(t, 1, calls)
	require.Equal(t, 1, executed)
	require.GreaterOrEqual(t, saved.attempts, 4)
	require.Equal(t, "unknown", saved.latest().Status)
	require.Len(t, saved.latest().Units, 2)
}

func TestCommentRepairOptimizationLeavesOtherExecutionPathsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, src, dst, kind string
		units, saves         int
	}{
		{name: "postgres target", src: "mysql", dst: "postgres", kind: "comment", units: 1, saves: 4},
		{name: "sqlserver source", src: "sqlserver", dst: "dameng", kind: "comment", units: 1, saves: 4},
		{name: "ordinary index", src: "mysql", dst: "dameng", kind: "index", units: 1, saves: 4},
		{name: "multiple comment statements", src: "mysql", dst: "dameng", kind: "comment", units: 2, saves: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, saved := newCommentOptimizationMigrator(t, tc.src, tc.dst)
			o, statements := commentOptimizationObject("ID")
			o.Kind = tc.kind
			if tc.units == 2 {
				_, next := commentOptimizationObject("NextID")
				statements = append(statements, next...)
			}
			executed := 0
			err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
				return observeIncrementalRepairSQL(ctx, statements, &executed)
			})
			require.NoError(t, err)
			require.Equal(t, tc.units, executed)
			require.Equal(t, tc.saves, saved.attempts)
			require.True(t, saved.latest().AllUnitsSucceeded())
		})
	}
}

func TestCommentRepairOptimizationRequiresIncrementalCapture(t *testing.T) {
	t.Run("capture disabled", func(t *testing.T) {
		m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
		m.cfg.CaptureRepair = false
		o, statements := commentOptimizationObject("ID")
		require.NoError(t, m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
			return observeIncrementalRepairSQL(ctx, statements, nil)
		}))
		require.Equal(t, 4, saved.attempts)
	})
	t.Run("legacy full manifest sink", func(t *testing.T) {
		m, saved := newCommentOptimizationMigrator(t, "mysql", "dameng")
		m.cfg.RepairObjectSink = nil
		fullSaves := 0
		var durable RepairManifest
		m.cfg.RepairSink = func(manifest RepairManifest) error {
			fullSaves++
			durable = cloneIncrementalRepairValue(t, manifest)
			return nil
		}
		o, statements := commentOptimizationObject("ID")
		require.NoError(t, m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
			return observeIncrementalRepairSQL(ctx, statements, nil)
		}))
		require.Equal(t, 4, fullSaves)
		require.Zero(t, saved.attempts)
		require.Len(t, durable.Objects, 1)
		require.Equal(t, "succeeded", durable.Objects[0].Status)
	})
}

func TestCommentRepairOptimization27463CommentsUseTwoSavesEachInOrder(t *testing.T) {
	const count = 27463
	m, _ := newCommentOptimizationMigrator(t, "mysql", "dameng")
	saves, executed := 0, 0
	var durable repair.Object
	m.cfg.RepairObjectSink = func(o repair.Object) error {
		saves++
		durable = cloneIncrementalRepairValue(t, o)
		return nil
	}
	for i := 0; i < count; i++ {
		o, statements := commentOptimizationObject(fmt.Sprintf("Column_%05d", i))
		err := m.captureRepairCall(context.Background(), o, statements, func(ctx context.Context) error {
			require.Equal(t, 2*i+1, saves)
			require.Equal(t, i, executed, "comments execute in source order without retries")
			require.Equal(t, o.TargetName, durable.TargetName)
			requireCommentRunningSnapshot(t, durable, statements[0].SQL)
			return observeIncrementalRepairSQL(ctx, statements, &executed)
		})
		require.NoError(t, err)
		require.Equal(t, 2*(i+1), saves)
		require.Equal(t, "succeeded", durable.Status)
	}
	require.Equal(t, count, executed)
	require.Equal(t, 2*count, saves)
	require.Len(t, m.repairs.objects, count)
}
