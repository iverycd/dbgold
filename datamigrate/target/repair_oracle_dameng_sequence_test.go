package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oracleDMSequenceRepairObject(w *DaMengWriter, seq source.SequenceInfo) repair.Object {
	obj := repair.Object{Kind: "sequence", Schema: "APP", TargetName: seq.Native.Name, Sequence: &seq}
	for _, statement := range w.dia.SequenceStatements("APP", seq, "") {
		obj.Units = append(obj.Units, oracleDMSequenceRepairUnit(statement.SQL))
	}
	return obj
}

func oracleDMSequenceRepairUnit(sqlText string) repair.Unit {
	return repair.Unit{SQL: sqlText, OriginalSQL: sqlText, Status: "pending", Selected: true}
}

func TestOracleDamengGeneratedRepairReusesCompatibleCreateUnits(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	obj := oracleDMSequenceRepairObject(w, s)
	expectOracleDMSequenceState(t, m, s, "51", []bool{true})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "40")
	var saved [][]repair.Unit
	units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, func(units []repair.Unit) error {
		saved = append(saved, units)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, saved, 2)
	for _, unit := range units {
		require.Equal(t, "succeeded", unit.Status)
		require.False(t, unit.Selected)
	}
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengGeneratedRepairRejectsMissingOrStaleReviewedAlter(t *testing.T) {
	for _, alter := range []string{"", `ALTER SEQUENCE "APP"."S" CURRENT VALUE 42`, `ALTER SEQUENCE "APP"."S" CURRENT VALUE 46`} {
		t.Run(alter, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			s := oracleDMNativeSequence()
			obj := oracleDMSequenceRepairObject(w, s)
			if alter != "" {
				obj.Units = append(obj.Units[:1], append([]repair.Unit{oracleDMSequenceRepairUnit(alter)}, obj.Units[1:]...)...)
			}
			expectOracleDMSequenceState(t, m, s, "2", []bool{false})
			expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
			units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
			require.ErrorContains(t, err, "刷新")
			require.True(t, IsOracleDamengSequencePreflight(err))
			for _, unit := range units {
				require.Equal(t, "pending", unit.Status, "read-only failure must not claim an uncertain execution")
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengGeneratedRepairExecutesExactReviewedAlterWithoutRewind(t *testing.T) {
	for _, current := range []string{"2", "52"} {
		t.Run(current, func(t *testing.T) {
			w, m := oracleDMRecoveryMock(t)
			s := oracleDMNativeSequence()
			alter := `ALTER SEQUENCE "APP"."S" CURRENT VALUE 47`
			obj := oracleDMSequenceRepairObject(w, s)
			obj.Units = []repair.Unit{oracleDMSequenceRepairUnit(alter)}
			expectOracleDMSequenceState(t, m, s, current, []bool{true})
			expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
			if current == "2" {
				m.ExpectExec(regexp.QuoteMeta(alter)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
			require.NoError(t, err)
			require.Equal(t, alter, units[0].SQL)
			require.Equal(t, "succeeded", units[0].Status)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestOracleDamengGeneratedRepairDoesNotCreateUnselectedTrigger(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	s.Native.Bindings = append(s.Native.Bindings, source.SequenceBinding{TableName: "T2", ColumnName: "ID2", TriggerName: "TRG2", Kind: "trigger", OnlyWhenNull: true})
	obj := oracleDMSequenceRepairObject(w, s)
	obj.Units[0].Selected, obj.Units[1].Selected = false, false
	expectOracleDMSequenceState(t, m, s, "51", []bool{false, false})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "40")
	expectOracleDMSequenceExtreme(m, "T2", "ID2", "MAX", "30")
	m.ExpectExec(regexp.QuoteMeta(obj.Units[2].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "pending", units[1].Status)
	require.Equal(t, "succeeded", units[2].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengEditedSequenceRepairExecutesSavedSQLVerbatim(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	obj := oracleDMSequenceRepairObject(w, oracleDMNativeSequence())
	obj.Units = obj.Units[:1]
	obj.Units[0].SQL = `ALTER SEQUENCE "APP"."S" CACHE 5`
	m.ExpectExec(regexp.QuoteMeta(`SET SCHEMA "APP"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(regexp.QuoteMeta(obj.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectClose()
	units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, obj.Units[0].SQL, units[0].SQL)
	require.Equal(t, "succeeded", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengGeneratedRepairPersistenceAfterSQLIsUnknown(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	obj := oracleDMSequenceRepairObject(w, s)
	obj.Units = []repair.Unit{oracleDMSequenceRepairUnit(`ALTER SEQUENCE "APP"."S" CURRENT VALUE 46`)}
	expectOracleDMSequenceState(t, m, s, "11", []bool{true})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
	m.ExpectExec(regexp.QuoteMeta(obj.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	saves := 0
	units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, func([]repair.Unit) error {
		saves++
		if saves == 2 {
			return errors.New("save result failed")
		}
		return nil
	})
	require.ErrorContains(t, err, "SQL 已执行")
	require.False(t, IsOracleDamengSequencePreflight(err))
	require.Equal(t, "unknown", units[0].Status)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleDamengGeneratedRepairMultipleAltersNeverRewind(t *testing.T) {
	w, m := oracleDMRecoveryMock(t)
	s := oracleDMNativeSequence()
	obj := oracleDMSequenceRepairObject(w, s)
	obj.Units = []repair.Unit{
		oracleDMSequenceRepairUnit(`ALTER SEQUENCE "APP"."S" CURRENT VALUE 52`),
		oracleDMSequenceRepairUnit(`ALTER SEQUENCE "APP"."S" CURRENT VALUE 47`),
	}
	expectOracleDMSequenceState(t, m, s, "2", []bool{true})
	expectOracleDMSequenceExtreme(m, "T", "ID", "MAX", "42")
	m.ExpectExec(regexp.QuoteMeta(obj.Units[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	units, err := ExecuteRepair(context.Background(), w, obj, obj.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.Equal(t, "succeeded", units[1].Status)
	require.NoError(t, m.ExpectationsWereMet())
}
