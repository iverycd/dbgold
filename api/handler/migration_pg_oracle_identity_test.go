package handler

import (
	"context"
	"encoding/json"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func pgOracleIdentityFixture(t *testing.T) (repair.Object, repair.Object) {
	t.Helper()
	p := pgoracle.SequencePlan{Schema: "APP", Name: "SEQ", Source: source.PGSequence{OID: 11, Name: "Seq", Schema: "Mixed", Increment: 1, Min: 1, Max: 1000, LastValue: 10, Cache: 1, Bindings: []source.PGSequenceBinding{{Schema: "Mixed", TableName: "Orders", ColumnName: "Id"}}}, Bindings: []pgoracle.Binding{{SourceTable: "Orders", SourceColumn: "Id", Table: "ORDERS", Column: "ID", Trigger: "TRG_ID"}}}
	statements, err := p.SequenceStatements()
	require.NoError(t, err)
	seq := repair.Object{ID: "seq", Kind: "sequence", Name: "Seq", Schema: "APP", TargetName: "SEQ", PGOracleSequence: &p, Units: []repair.Unit{{ID: "create", SQL: statements[0].SQL, OriginalSQL: statements[0].SQL, Status: "failed"}}}
	copy := p
	sql := p.TriggerSQL(p.Bindings[0])
	trigger := repair.Object{ID: "trigger", Kind: "trigger", Name: "TRG_ID", Schema: "APP", TargetName: "TRG_ID", Table: "ORDERS", PGOracleSequence: &copy, Status: "failed", Units: []repair.Unit{{ID: "trigger", SQL: sql, OriginalSQL: sql, Status: "failed", Selected: true}}}
	return seq, trigger
}

func TestPGOracleRepairProgressPersistsDependentIdentityAtomically(t *testing.T) {
	db := repairPersistenceDB(t)
	require.NoError(t, db.AutoMigrate(&store.MigrationRepairAttempt{}))
	seq, trigger := pgOracleIdentityFixture(t)
	for _, obj := range []repair.Object{seq, trigger} {
		require.NoError(t, db.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: obj.ID, ObjectJSON: repairJSON(obj), Version: 1}).Error)
	}
	attempt := &store.MigrationRepairAttempt{ID: "attempt", JobID: "job", ObjectID: "seq", RequestID: "r", RequestHash: "h", Status: "running", UnitsJSON: "old"}
	require.NoError(t, db.Create(attempt).Error)
	seq.PGOracleSequence.Identity = &pgoracle.SequenceIdentity{ObjectID: 91, Fingerprint: seq.PGOracleSequence.Fingerprint()}
	units := append([]repair.Unit{}, seq.Units...)
	units[0].Status = "succeeded"
	require.NoError(t, persistRepairProgress(attempt, seq, units, nil))
	var row store.MigrationRepairObject
	require.NoError(t, db.First(&row, "job_id = ? AND object_id = ?", "job", "trigger").Error)
	var got repair.Object
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &got))
	require.Equal(t, int64(91), got.PGOracleSequence.Identity.ObjectID)
	require.Equal(t, "failed", got.Status)
	require.Equal(t, "failed", got.Units[0].Status)
	require.Equal(t, 2, row.Version)
	// Successful object evidence stays available despite the selected list being empty.
	ctx, err := pgOracleRecoveryEvidence(context.Background(), "job", repair.Object{SourceTable: "Orders", Table: "ORDERS"}, "postgres", "oracle")
	require.NoError(t, err)
	evidence := datamigrate.PGOracleSequenceEvidence(ctx)
	require.Len(t, evidence, 2)
	require.Equal(t, int64(91), evidence[0].Identity.ObjectID)
}

func TestPGOracleConflictingIdentityRollsBackSuccessPersistence(t *testing.T) {
	db := repairPersistenceDB(t)
	require.NoError(t, db.AutoMigrate(&store.MigrationRepairAttempt{}))
	seq, trigger := pgOracleIdentityFixture(t)
	trigger.PGOracleSequence.Identity = &pgoracle.SequenceIdentity{ObjectID: 99, Fingerprint: trigger.PGOracleSequence.Fingerprint()}
	for _, obj := range []repair.Object{seq, trigger} {
		require.NoError(t, db.Create(&store.MigrationRepairObject{JobID: "job", ObjectID: obj.ID, ObjectJSON: repairJSON(obj), Version: 1}).Error)
	}
	attempt := &store.MigrationRepairAttempt{ID: "attempt", JobID: "job", ObjectID: "seq", RequestID: "r", RequestHash: "h", Status: "running", UnitsJSON: "old"}
	require.NoError(t, db.Create(attempt).Error)
	seq.PGOracleSequence.Identity = &pgoracle.SequenceIdentity{ObjectID: 91, Fingerprint: seq.PGOracleSequence.Fingerprint()}
	units := append([]repair.Unit{}, seq.Units...)
	units[0].Status = "succeeded"
	require.ErrorContains(t, persistRepairProgress(attempt, seq, units, nil), "冲突身份")
	var row store.MigrationRepairObject
	require.NoError(t, db.First(&row, "job_id = ? AND object_id = ?", "job", "seq").Error)
	var got repair.Object
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &got))
	require.Nil(t, got.PGOracleSequence.Identity)
	require.Equal(t, "failed", got.Units[0].Status)
	var saved store.MigrationRepairAttempt
	require.NoError(t, db.First(&saved, "id = ?", "attempt").Error)
	require.Equal(t, "old", saved.UnitsJSON)
}

func TestPGOracleIdentityPropagationDoesNotAcceptChangedBindings(t *testing.T) {
	seq, trigger := pgOracleIdentityFixture(t)
	seq.PGOracleSequence.Identity = &pgoracle.SequenceIdentity{ObjectID: 91, Fingerprint: seq.PGOracleSequence.Fingerprint()}
	copy := append([]pgoracle.Binding{}, trigger.PGOracleSequence.Bindings...)
	copy[0].Column = "OTHER"
	trigger.PGOracleSequence.Bindings = copy
	_, err := inheritPGOracleSequenceIdentity(&trigger, *seq.PGOracleSequence)
	require.ErrorContains(t, err, "冻结方案不一致")
	require.Nil(t, trigger.PGOracleSequence.Identity)
}

func TestPGOracleRepairScopeAcceptsOnlyFrozenSequenceAndTrigger(t *testing.T) {
	seq, trigger := pgOracleIdentityFixture(t)
	for _, obj := range []repair.Object{seq, trigger} {
		require.True(t, obj.CanExecuteDDL())
		require.False(t, obj.CanEditDDL())
		require.NoError(t, validateRepairObjectScope(obj, obj.Units, "oracle"))
		require.Error(t, validateRepairObjectScope(obj, obj.Units, "mysql"))
		changed := append([]repair.Unit{}, obj.Units...)
		changed[0].SQL += " -- altered"
		require.Error(t, validateRepairObjectScope(obj, changed, "oracle"))
	}
}
