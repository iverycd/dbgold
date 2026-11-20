package target

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestManualRepairRunsSelectedEditorSQLVerbatim(t *testing.T) {
	exact := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		if expected != actual {
			return fmt.Errorf("expected %q, got %q", expected, actual)
		}
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(exact))
	require.NoError(t, err)
	defer db.Close()
	w := &DaMengWriter{db: db, schema: "APP", dia: dialect.NewDaMeng()}
	const view = "  -- 手工修复\nCREATE OR REPLACE force VIEW \"OTHER\".\"MixedView\" AS\n  SELECT 'a;b' FROM DUAL;\n"
	const block = "BEGIN\n UPDATE OTHER.T SET n = 2;\n NULL; END;  "
	units := []repair.Unit{
		{SQL: "already done", Status: "succeeded", Selected: true},
		{SQL: view, OriginalSQL: "original view", Selected: true},
		{SQL: "not selected", Status: "pending"},
		{SQL: block, Selected: true},
		{SQL: "after error", Status: "pending", Selected: true},
	}
	mock.ExpectExec(`SET SCHEMA "APP"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(view).WillReturnResult(sqlmock.NewResult(0, 0))
	const targetError = "Error -2007: 第 101 行，第 41 列附近出现错误"
	mock.ExpectExec(block).WillReturnError(errors.New(targetError))
	mock.ExpectClose()
	var snapshots [][]repair.Unit
	result, err := ExecuteRepair(context.Background(), w, repair.Object{ManualSQL: true, Kind: "view", Schema: "APP", TargetName: "original"}, units, func(steps []repair.Unit) error {
		snapshots = append(snapshots, steps)
		return nil
	})
	require.ErrorContains(t, err, targetError)
	require.False(t, result[0].Selected)
	require.Equal(t, "succeeded", result[1].Status)
	require.Equal(t, "pending", result[2].Status)
	require.Equal(t, "failed", result[3].Status)
	require.Equal(t, targetError, result[3].Error)
	require.Equal(t, "pending", result[4].Status)
	require.Len(t, snapshots, 4)
	require.Equal(t, "running", snapshots[0][1].Status)
	require.Equal(t, block, snapshots[3][3].SQL)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManualRepairUsesNativeDamengSessionWithoutGeneratedViewPlan(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	w := &DaMengNativeWriter{&DaMengWriter{db: db, schema: "MixedOwner", dia: &damengNativeDialect{dialect.NewDaMeng()}}}
	mock.ExpectExec(`SET SCHEMA "MixedOwner"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SELECT 1 FROM DUAL").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()
	result, err := ExecuteRepair(context.Background(), w, repair.Object{ManualSQL: true, Kind: "view", Schema: "MixedOwner"}, []repair.Unit{{Selected: true, SQL: "SELECT 1 FROM DUAL"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManualRepairPGUsesDriverTransactionsForEveryEditor(t *testing.T) {
	const index = "CREATE INDEX CONCURRENTLY ix ON other.one(id)"
	for _, kind := range []string{"table", "view", "trigger"} {
		t.Run(kind, func(t *testing.T) {
			w, mock := repairPGMock(t)
			mock.ExpectExec("SET search_path").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(regexp.QuoteMeta(index)).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("UPDATE other.two").WillReturnError(errors.New("missing column"))
			mock.ExpectClose()
			obj := repair.Object{ManualSQL: true, Kind: kind, Schema: "app"}
			if kind == "trigger" {
				obj.ManualTrigger = &repair.ManualTriggerPlan{}
			}
			units := []repair.Unit{{Selected: true, SQL: index}, {Selected: true, SQL: "UPDATE other.two SET id=2"}}
			result, err := ExecuteRepair(context.Background(), w, obj, units, nil)
			require.ErrorContains(t, err, "missing column")
			require.Equal(t, "succeeded", result[0].Status)
			require.Equal(t, "failed", result[1].Status)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestManualRepairSQLServerSubmitsUserTransactionAsOneCall(t *testing.T) {
	w, mock := sqlServerMock(t)
	const transaction = "BEGIN TRANSACTION;\nUPDATE other.t SET id=2;\nCOMMIT TRANSACTION;"
	mock.ExpectExec(regexp.QuoteMeta(transaction)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectClose()
	obj := repair.Object{ManualSQL: true, Kind: "view", Schema: "dbo"}
	units := []repair.Unit{{Selected: true, SQL: transaction}}
	result, err := ExecuteRepair(context.Background(), w, obj, units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManualRepairPassesWholeDocumentToOscar(t *testing.T) {
	bridge := &fakeOscarBridge{}
	w := newFakeOscarWriter(bridge)
	const document = "  UPDATE OTHER.T SET n = 2;\nUPDATE OTHER.T SET n = 3;\n"
	result, err := ExecuteRepair(context.Background(), w, repair.Object{ManualSQL: true, Schema: "业务"}, []repair.Unit{{Selected: true, SQL: document}}, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result[0].Status)
	require.Equal(t, []string{"exec"}, bridge.calls)
	require.Equal(t, document, bridge.lastSQL)
}
