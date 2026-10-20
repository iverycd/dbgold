package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func oracleDMSequenceRecorder(t *testing.T) (*Migrator, repair.Object, []dialect.Statement) {
	t.Helper()
	_, _, m := oracleDMFixture("schema_only")
	m.repairs = &repairRecorder{}
	seq := source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValueText: "1",
		Native: &source.NativeSequenceInfo{Name: "S", IncrementBy: "5", MinValue: "1", MaxValue: "1000", CacheSize: "0",
			Bindings: []source.SequenceBinding{
				{TableName: "T", ColumnName: "ID", Kind: "trigger", TriggerName: "TRG_T", OnlyWhenNull: true},
				{TableName: "U", ColumnName: "ID", Kind: "trigger", TriggerName: "TRG_U", OnlyWhenNull: true},
			},
		},
	}
	statements := dialect.NewDaMeng().SequenceStatements("APP", seq, "")
	require.Len(t, statements, 3)
	return m, repair.Object{Kind: "sequence", Name: "S", Table: "T", TargetName: "S", Sequence: &seq}, statements
}

func requireOracleDMCanonicalSequenceUnits(t *testing.T, object repair.Object, statements []dialect.Statement) {
	t.Helper()
	require.Equal(t, dialect.JoinSQL(statements), object.DDL)
	require.Len(t, object.Units, len(statements), "observations must update canonical units rather than append duplicates")
	for i, statement := range statements {
		require.Equal(t, fmt.Sprintf("step-%d", i+1), object.Units[i].ID)
		require.Equal(t, statement.SQL, object.Units[i].SQL)
		require.Equal(t, statement.SQL, object.Units[i].OriginalSQL)
		require.False(t, object.Units[i].Destructive)
	}
}

func TestOracleDamengSequenceRecordPreflightLeavesCanonicalUnitsRetryable(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			m, object, statements := oracleDMSequenceRecorder(t)
			cause := errors.New("existing sequence has incompatible increment")
			preflight := target.OracleDamengSequencePreflight(cause)
			if wrapped {
				preflight = fmt.Errorf("inspect sequence: %w", preflight)
			}
			err := m.captureRepairCall(context.Background(), object, statements, func(context.Context) error { return preflight })
			require.ErrorIs(t, err, cause)
			require.True(t, target.IsOracleDamengSequencePreflight(err))
			require.Len(t, m.repairs.objects, 1)
			recorded := m.repairs.objects[0]
			requireOracleDMCanonicalSequenceUnits(t, recorded, statements)
			require.Equal(t, "failed", recorded.Status, "read-only preflight failure must not imply an unknown execution")
			require.Equal(t, preflight.Error(), recorded.Error)
			require.True(t, recorded.CanEditDDL())
			require.False(t, recorded.RequiresReview)
			for _, unit := range recorded.Units {
				require.Equal(t, "pending", unit.Status)
				require.Empty(t, unit.Error)
				require.True(t, unit.Selected, "all unattempted canonical statements remain available for retry")
			}
		})
	}
}

func TestOracleDamengSequenceRecordReusedEventsCompleteMatchingCanonicalUnits(t *testing.T) {
	m, object, statements := oracleDMSequenceRecorder(t)
	err := m.captureRepairCall(context.Background(), object, statements, func(ctx context.Context) error {
		for i, statement := range statements {
			require.NoError(t, repair.Observe(ctx, statement.SQL, "reused", nil))
			recorded := m.repairs.objects[0]
			requireOracleDMCanonicalSequenceUnits(t, recorded, statements)
			for j, unit := range recorded.Units {
				if j <= i {
					require.Equal(t, "succeeded", unit.Status)
					require.False(t, unit.Selected)
				} else {
					require.Equal(t, "pending", unit.Status)
					require.True(t, unit.Selected)
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	recorded := m.repairs.objects[0]
	requireOracleDMCanonicalSequenceUnits(t, recorded, statements)
	require.Equal(t, "succeeded", recorded.Status)
	require.True(t, recorded.AllUnitsSucceeded())
	for _, unit := range recorded.Units {
		require.False(t, unit.Selected)
	}
}

func TestOracleDamengSequenceRecordReplacesCreateWithObservedSafeStart(t *testing.T) {
	m, object, canonical := oracleDMSequenceRecorder(t)
	original := oracleDMFlowSnapshot(*object.Sequence)
	created := *object.Sequence
	created.StartValueText = "41"
	actual := m.writer.Dialect().SequenceStatements("APP", created, "")
	require.NotEqual(t, canonical[0].SQL, actual[0].SQL)
	persistedBeforeExecution := false
	m.cfg.RepairObjectSink = func(recorded repair.Object) error {
		if len(recorded.Units) > 0 && recorded.Units[0].Status == "running" {
			persistedBeforeExecution = true
			requireOracleDMCanonicalSequenceUnits(t, recorded, actual)
			require.Equal(t, "41", recorded.Sequence.StartValueText)
		}
		return nil
	}
	err := m.captureRepairCall(context.Background(), object, canonical, func(ctx context.Context) error {
		for _, statement := range actual {
			require.NoError(t, repair.Observe(ctx, statement.SQL, "running", nil))
			require.NoError(t, repair.Observe(ctx, statement.SQL, "succeeded", nil))
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, persistedBeforeExecution, "the actual CREATE must replace its pending unit before execution")
	recorded := m.repairs.objects[0]
	requireOracleDMCanonicalSequenceUnits(t, recorded, actual)
	require.Equal(t, "41", recorded.Sequence.StartValueText)
	require.Equal(t, "succeeded", recorded.Status)
	require.True(t, recorded.AllUnitsSucceeded())
	creates := 0
	for _, unit := range recorded.Units {
		if strings.HasPrefix(unit.SQL, "CREATE SEQUENCE ") {
			creates++
		}
	}
	require.Equal(t, 1, creates)
	require.Equal(t, original, *object.Sequence, "recording the target start must preserve source metadata")
	require.Contains(t, canonical[0].SQL, "START WITH 1 INCREMENT BY 5")
}

func TestOracleDamengSequenceRecordExecutionUncertaintyRemainsUnknown(t *testing.T) {
	for _, terminalEvent := range []string{"unknown", ""} {
		t.Run("terminal="+terminalEvent, func(t *testing.T) {
			m, object, statements := oracleDMSequenceRecorder(t)
			networkErr := &net.OpError{Op: "write", Net: "tcp", Err: io.ErrUnexpectedEOF}
			err := m.captureRepairCall(context.Background(), object, statements, func(ctx context.Context) error {
				require.NoError(t, repair.Observe(ctx, statements[0].SQL, "reused", nil))
				require.NoError(t, repair.Observe(ctx, statements[1].SQL, "running", nil))
				if terminalEvent != "" {
					require.NoError(t, repair.Observe(ctx, statements[1].SQL, terminalEvent, networkErr))
				}
				return networkErr
			})
			require.ErrorIs(t, err, networkErr)
			require.False(t, target.IsOracleDamengSequencePreflight(err))
			recorded := m.repairs.objects[0]
			requireOracleDMCanonicalSequenceUnits(t, recorded, statements)
			// Existing retry gates block an unknown object or execution unit.
			// Recognizing reuse/preflight must never remove that evidence.
			require.Equal(t, "unknown", recorded.Status)
			require.Equal(t, "succeeded", recorded.Units[0].Status)
			require.False(t, recorded.Units[0].Selected)
			require.Equal(t, "unknown", recorded.Units[1].Status)
			require.True(t, recorded.Units[1].Selected)
			require.Equal(t, "pending", recorded.Units[2].Status)
			require.True(t, recorded.Units[2].Selected)
		})
	}
}

func TestOracleDamengSequenceRecordMissingTriggerFailurePreservesReuse(t *testing.T) {
	m, object, statements := oracleDMSequenceRecorder(t)
	executionErr := errors.New("create trigger privilege denied")
	err := m.captureRepairCall(context.Background(), object, statements, func(ctx context.Context) error {
		for _, statement := range statements[:2] {
			require.NoError(t, repair.Observe(ctx, statement.SQL, "reused", nil))
		}
		require.NoError(t, repair.Observe(ctx, statements[2].SQL, "running", nil))
		require.NoError(t, repair.Observe(ctx, statements[2].SQL, "failed", executionErr))
		return executionErr
	})
	require.ErrorIs(t, err, executionErr)
	require.False(t, target.IsOracleDamengSequencePreflight(err))
	recorded := m.repairs.objects[0]
	requireOracleDMCanonicalSequenceUnits(t, recorded, statements)
	require.Equal(t, "failed", recorded.Status)
	for _, unit := range recorded.Units[:2] {
		require.Equal(t, "succeeded", unit.Status)
		require.Empty(t, unit.Error)
		require.False(t, unit.Selected, "matching existing objects must not be selected for recreation")
	}
	require.Equal(t, "failed", recorded.Units[2].Status)
	require.Equal(t, executionErr.Error(), recorded.Units[2].Error)
	require.True(t, recorded.Units[2].Selected)
}
