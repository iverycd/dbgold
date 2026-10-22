package target

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func testRedefinitionView() damengnative.MaterializedHistoryView {
	return damengnative.MaterializedHistoryView{Name: "MV", Table: "Storage", Staging: "Immutable", Redefinition: &damengnative.MaterializedRedefinitionPlan{WorkingTable: "Work", SwapName: "Swap"}}
}

func expectRedefinitionState(m sqlmock.Sqlmock, bound string, names map[string]int64) {
	m.ExpectQuery(`SELECT T.ID,T.NAME FROM SYS.SYSOBJECTS V`).WithArgs("Owner", "MV").WillReturnRows(sqlmock.NewRows([]string{"ID", "NAME"}).AddRow(500, bound))
	r := sqlmock.NewRows([]string{"NAME", "ID"})
	for _, name := range []string{"Storage", "Work", "Swap"} {
		if id := names[name]; id != 0 {
			r.AddRow(name, id)
		}
	}
	m.ExpectQuery(`SELECT NAME,ID FROM SYS.SYSOBJECTS`).WithArgs("Owner", "Storage", "Work", "Swap").WillReturnRows(r)
}

func TestMaterializedRedefinitionRecoversEveryLostRenameResponse(t *testing.T) {
	states := []struct {
		bound string
		names map[string]int64
		from  string
		to    string
	}{
		{"Work", map[string]int64{"Storage": 600, "Work": 500}, "Storage", "Swap"}, // FINISH response lost
		{"Work", map[string]int64{"Work": 500, "Swap": 600}, "Work", "Storage"},    // first RENAME response lost
		{"Storage", map[string]int64{"Storage": 500, "Swap": 600}, "Swap", "Work"}, // second RENAME response lost
		{"Storage", map[string]int64{"Storage": 500, "Work": 600}, "", ""},         // third RENAME response lost
	}
	for first := range states {
		t.Run(states[first].bound+states[first].to, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			conn, err := db.Conn(context.Background())
			require.NoError(t, err)
			defer conn.Close()
			for _, s := range states[first:] {
				expectRedefinitionState(m, s.bound, s.names)
				if s.from != "" {
					m.ExpectExec(`ALTER TABLE "Owner"."` + s.from + `" RENAME TO "` + s.to + `"`).WillReturnResult(sqlmock.NewResult(0, 0))
				}
			}
			require.NoError(t, historyNormalizeRedefinitionNames(context.Background(), conn, "Owner", testRedefinitionView()))
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestMaterializedRedefinitionUnknownPhysicalBindingPerformsNoWrite(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	m.ExpectQuery(`SELECT T.ID,T.NAME FROM SYS.SYSOBJECTS V`).WithArgs("Owner", "MV").WillReturnRows(sqlmock.NewRows([]string{"ID", "NAME"}).AddRow(500, "TargetOnly"))
	err = historyNormalizeRedefinitionNames(context.Background(), conn, "Owner", testRedefinitionView())
	require.ErrorContains(t, err, "不在冻结重定义名称中")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedRedefinitionAbortRequiresFrozenDependencies(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	expectRedefinitionState(m, "Storage", map[string]int64{"Storage": 500, "Work": 600})
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS`).WithArgs("Owner", "MVIEW$_Storage").WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(1))
	m.ExpectQuery(`SELECT COUNT\(DISTINCT REFERENCED_NAME\)`).WithArgs("Owner", "MVIEW$_Storage", "Owner", "Storage", "Work").WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(1))
	require.ErrorContains(t, historyReconcileRedefinition(context.Background(), conn, "Owner", testRedefinitionView()), "依赖不属于冻结工作表与原存储")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedRedefinitionReadOnlyPreflightConflict(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "Owner"}}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_OBJECTS WHERE OWNER='SYS'`).WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(2))
	m.ExpectQuery(`SELECT CASE WHEN USER IN`).WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(1))
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_OBJECTS WHERE OWNER=\?`).WithArgs("Owner", "Swap", "MVIEW$_Storage", "MLOG$_Work", "MTRG$_Work").WillReturnRows(sqlmock.NewRows([]string{"N"}).AddRow(1))
	require.ErrorContains(t, w.validateHistoryRedefinition(context.Background(), []damengnative.MaterializedHistoryView{testRedefinitionView()}), "临时名称与目标已有对象冲突")
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMaterializedRedefinitionPreflightChecksEntireFrozenPlan(t *testing.T) {
	for _, name := range []string{"Swap", "MVIEW$_Storage", "MLOG$_Work", "MTRG$_Work"} {
		t.Run(name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			w := &DaMengNativeWriter{DaMengWriter: &DaMengWriter{db: db, schema: "Owner"}}
			err = w.validateHistoryRedefinition(context.Background(), []damengnative.MaterializedHistoryView{testRedefinitionView()}, []damengnative.Object{{Schema: "Owner", Name: name, Kind: "table"}})
			require.ErrorContains(t, err, "本次计划对象")
			require.NoError(t, m.ExpectationsWereMet()) // No query or write was needed.
		})
	}
	require.NoError(t, historyRedefinitionPlanConflicts("Owner", []damengnative.MaterializedHistoryView{testRedefinitionView()}, []damengnative.Object{{Schema: "Other", Name: "Swap", Kind: "table"}, {Schema: "Owner", Name: "swap", Kind: "table"}}))
}

func TestMaterializedRedefinitionCancellationIsNotReplayed(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	cancel()
	err = historyRebaseStorage(ctx, conn, "Owner", testRedefinitionView())
	require.True(t, errors.Is(err, context.Canceled), err)
	require.NoError(t, m.ExpectationsWereMet())
}
