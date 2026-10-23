package target

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeEmptyCopyRestoresInterruptedDefaultWithoutDataTransaction(t *testing.T) {
	w, m := mockNativeDM(t)
	expectCopyDefaultState(m, `CREATE TABLE "MixedOwner"."T"("V" INT NOT NULL)`, nil)
	expectCopyDefaultRestored(m)
	require.NoError(t, w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), nil))
	require.NoError(t, m.ExpectationsWereMet(), "an empty recovery must restore missing defaults without DROP, INSERT or a transaction")
}

func TestNativeEmptyCopyLeavesMatchingDefaultsUntouched(t *testing.T) {
	w, m := mockNativeDM(t)
	expectCopyDefaultState(m, copyDefaultDDL, `"MixedOwner"."Seq".NEXTVAL`)
	require.NoError(t, w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), nil))
	require.NoError(t, m.ExpectationsWereMet(), "valid defaults must require only read-only inspection")
}

func TestNativeEmptyCopyRejectsChangedDefaultBeforeDDL(t *testing.T) {
	w, m := mockNativeDM(t)
	expectCopyDefaultState(m, copyDefaultDDL, `"MixedOwner"."Different".NEXTVAL`)
	require.ErrorContains(t, w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), nil), "偏离冻结原生计划")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNativeEmptyCopyReportsRestorationFailureAndCanRetry(t *testing.T) {
	for attempt := 0; attempt < 2; attempt++ {
		// The failed attempt discards its changed session. A retry acquires a
		// fresh connection and reconciles the same persisted missing default.
		w, m := mockNativeDM(t)
		expectCopyDefaultState(m, `CREATE TABLE "MixedOwner"."T"("V" INT NOT NULL)`, nil)
		if attempt == 0 {
			m.ExpectExec(`SET SCHEMA "MixedOwner"`).WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec(`ALTER TABLE .* SET DEFAULT ON NULL`).WillReturnError(errors.New("session interrupted"))
			m.ExpectClose()
		} else {
			expectCopyDefaultRestored(m)
		}
		err := w.CopyDamengNativeBatch(context.Background(), nativeDefaultTable(), nil)
		if attempt == 0 {
			require.ErrorContains(t, err, "按整表计划补迁")
		} else {
			require.NoError(t, err)
		}
		require.NoError(t, m.ExpectationsWereMet())
	}
}

func TestNativeEmptyCopyOnlyRestoresMissingMembersOfFrozenDefaultSet(t *testing.T) {
	w, m := mockNativeDM(t)
	table := nativeDefaultTable()
	table.Columns = append(table.Columns, damengnative.Column{Name: "Other", SQLType: "INT", Writable: true, DefaultOnNull: true, DefaultSQL: "17"})
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("T", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "MixedOwner"."T"("V" INT DEFAULT ON NULL "MixedOwner"."Seq".NEXTVAL, "Other" INT)`))
	m.ExpectQuery(`SELECT COLUMN_NAME,DATA_DEFAULT`).WithArgs("MixedOwner", "T").WillReturnRows(sqlmock.NewRows([]string{"column", "default"}).AddRow("V", `"MixedOwner"."Seq".NEXTVAL`).AddRow("Other", nil))
	m.ExpectExec(`SET SCHEMA "MixedOwner"`).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec(`ALTER TABLE "MixedOwner"\."T" ALTER COLUMN "Other" SET DEFAULT ON NULL 17`).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("T", "MixedOwner").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "MixedOwner"."T"("V" INT DEFAULT ON NULL "MixedOwner"."Seq".NEXTVAL, "Other" INT DEFAULT ON NULL 17)`))
	m.ExpectQuery(`SELECT DATA_DEFAULT`).WithArgs("MixedOwner", "T", "Other").WillReturnRows(sqlmock.NewRows([]string{"default"}).AddRow("17"))
	m.ExpectClose()
	require.NoError(t, w.CopyDamengNativeBatch(context.Background(), table, nil))
	require.NoError(t, m.ExpectationsWereMet(), "the present sequence default must not be dropped or reset")
}

func TestNativeEmptyCopyStillRejectsWrongSchemaBeforeInspection(t *testing.T) {
	w, m := mockNativeDM(t)
	table := nativeDefaultTable()
	table.Schema = "OtherOwner"
	require.ErrorContains(t, w.CopyDamengNativeBatch(context.Background(), table, nil), "schema 与目标不一致")
	require.NoError(t, m.ExpectationsWereMet())
}
