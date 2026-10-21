package cdc

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestCDCTargetTriggerGuard(t *testing.T) {
	for _, mode := range []string{"none", "unrelated", "selected", "catalog_error"} {
		t.Run(mode, func(t *testing.T) {
			db, m := newTargetMetadataMock(t)
			expect := m.ExpectQuery(targetBusinessTriggersSQL).WithArgs("Target")
			if mode == "catalog_error" {
				expect.WillReturnError(errors.New("catalog denied"))
			} else {
				rows := sqlmock.NewRows([]string{"table", "trigger"})
				if mode == "unrelated" {
					rows.AddRow("Unrelated", "audit")
				}
				if mode == "selected" {
					rows.AddRow("orders", "audit_orders")
				}
				expect.WillReturnRows(rows)
			}
			err := checkTargetBusinessTriggers(context.Background(), db, Config{TargetSchema: "Target", LowerCaseNames: true}, []TableInfo{{Name: "Orders"}})
			if mode == "selected" || mode == "catalog_error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestCDCApplyRejectsNewTriggerBeforeWritingOrCheckpointing(t *testing.T) {
	db, m := newTargetMetadataMock(t)
	a := &PostgresApplier{db: db, schema: "Target", lower: true}
	m.ExpectBegin()
	m.ExpectExec(`LOCK TABLE "Target"."orders" IN ROW EXCLUSIVE MODE`).WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery(targetBusinessTriggersSQL).WithArgs("Target").WillReturnRows(sqlmock.NewRows([]string{"table", "trigger"}).AddRow("orders", "audit_orders"))
	m.ExpectRollback()
	_, err := a.Apply(context.Background(), []Change{{Action: "insert", Table: &TableInfo{Name: "Orders"}}}, Position{File: "mysql-bin.000001", Pos: 99})
	require.ErrorContains(t, err, "audit_orders")
	require.NoError(t, m.ExpectationsWereMet())
}
