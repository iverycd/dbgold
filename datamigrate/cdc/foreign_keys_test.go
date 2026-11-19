package cdc

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestLoadSourceInboundForeignKeysPreservesCompositeColumnPairs(t *testing.T) {
	db, mock := newSequenceMock(t)
	mock.ExpectQuery(sourceInboundForeignKeysSQL).WithArgs("SourceDB").WillReturnRows(
		sqlmock.NewRows([]string{"parent", "child_schema", "child", "name", "column", "referenced_column", "update_rule", "delete_rule"}).
			AddRow("Parent", "SourceDB", "Child", "MixedFK", "ParentB", "B", "CASCADE", "SET NULL").
			AddRow("Parent", "SourceDB", "Child", "MixedFK", "ParentA", "A", "CASCADE", "SET NULL").
			AddRow("Ignored", "SourceDB", "Elsewhere", "AnotherFK", "ID", "ID", "NO ACTION", "NO ACTION"))
	tables := []TableInfo{{Name: "Parent"}, {Name: "NoChild"}}
	require.NoError(t, LoadSourceInboundForeignKeys(context.Background(), db, "SourceDB", tables))
	require.True(t, tables[0].ForeignKeysKnown)
	require.True(t, tables[1].ForeignKeysKnown)
	require.Empty(t, tables[1].InboundForeignKeys)
	require.Equal(t, []foreignKey{{
		Schema: "SourceDB", Table: "Child", Name: "MixedFK", Columns: []string{"ParentB", "ParentA"}, RefColumns: []string{"B", "A"},
		OnUpdate: "CASCADE", OnDelete: "SET NULL",
	}}, tables[0].InboundForeignKeys)
}

func TestDamengIdentityReplacementRejectsInboundForeignKeyAcrossSchemas(t *testing.T) {
	db, mock := newSequenceMock(t)
	row := damengTestRow(t, db, true, false)
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`LOCK TABLE "APP"."ITEMS" IN EXCLUSIVE MODE`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(damengInboundConstraintSQL).WithArgs("APP", "ITEMS").WillReturnRows(
		sqlmock.NewRows([]string{"OWNER", "TABLE_NAME", "CONSTRAINT_NAME"}).AddRow("OTHER", "CHILD", "CHILD_FK"))
	mock.ExpectRollback()
	err = row.apply(context.Background(), tx, Change{Action: "update", Before: []any{int64(7), "old"}, After: []any{int64(9), "new"}}, func(string) error {
		t.Fatal("no identity insert expected")
		return nil
	})
	require.ErrorContains(t, err, "OTHER.CHILD")
	require.NoError(t, tx.Rollback())
}

func TestDamengRejectsSourceCascadeAndUnprovenLegacyLocatorChange(t *testing.T) {
	db, _ := newSequenceMock(t)
	row := damengTestRow(t, db, false, false)
	row.applier.cfg.SourceDatabase = "SourceDB"
	row.applier.cfg.TableNames = []string{"Items", "Detail"}
	row.applier.checkpointGuard = &targetCheckpointGuard{position: Position{File: "bin.000001", Pos: 50}}
	before, after := []any{int64(7), "old"}, []any{int64(9), "new"}
	change := Change{Action: "update", Table: row.table, Before: before, After: after}
	require.ErrorContains(t, row.checkForeignKeyUpdate(change), "旧检查点缺少可信的入向外键信息")
	row.table.ForeignKeysKnown = true
	row.table.InboundForeignKeys = []foreignKey{{Schema: "SourceDB", Table: "Detail", Name: "ParentFK", RefColumns: []string{"Id"}, OnUpdate: "CASCADE"}}
	require.ErrorContains(t, row.checkForeignKeyUpdate(change), "ON UPDATE CASCADE")
	row.applier.cfg.TableNames = []string{"Items"}
	require.NoError(t, row.checkForeignKeyUpdate(change))
}
