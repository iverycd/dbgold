package cdc

import (
	"context"
	"encoding/hex"
	"regexp"
	"testing"

	"dbgold/datamigrate/target"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestCDCSpatialPreparationCachesOnlySpatialTables(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	a := &PostgresApplier{db: db, schema: "App", lower: true, spatialCaps: &target.PGSpatialCapabilities{Schema: "Spatial"}}
	tables := []TableInfo{
		{Name: "Ordinary", Columns: []string{"Id"}, ColumnTypes: []string{"INT"}},
		{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"POINT"}},
	}
	mock.ExpectQuery(`SELECT tn.nspname,t.typname,a.atttypmod`).WithArgs("App", "shapes", "shape").WillReturnRows(sqlmock.NewRows([]string{"schema", "type", "typmod"}).AddRow("Spatial", "geometry", -1))
	mock.ExpectQuery(`SELECT pg_get_constraintdef`).WithArgs("App", "shapes", "shape").WillReturnRows(sqlmock.NewRows([]string{"check"}))
	require.NoError(t, a.PrepareSpatial(context.Background(), tables))
	require.NoError(t, a.PrepareSpatial(context.Background(), tables))
	require.True(t, a.spatialTables["Shapes"])
	require.False(t, a.spatialTables["Ordinary"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialInspectDoesNotEnableExtension(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	a := &PostgresApplier{db: db, schema: "App"}
	mock.ExpectQuery(`SELECT n.nspname,EXISTS`).WillReturnRows(sqlmock.NewRows([]string{"schema", "postgis", "visible"}))
	err = a.inspectSpatial(context.Background(), []TableInfo{{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"GEOMETRY"}}})
	require.ErrorContains(t, err, "只读")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialRejectsExistingPlainTextColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	a := &PostgresApplier{db: db, schema: "App", spatialCaps: &target.PGSpatialCapabilities{Schema: "Spatial"}}
	mock.ExpectQuery(`SELECT tn.nspname,t.typname,a.atttypmod`).WithArgs("App", "Shapes", "Shape").WillReturnRows(sqlmock.NewRows([]string{"schema", "type", "typmod"}).AddRow("pg_catalog", "text", -1))
	err = a.PrepareSpatial(context.Background(), []TableInfo{{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"POINT"}}})
	require.ErrorContains(t, err, "pg_catalog.text")
	require.False(t, a.spatialTables["Shapes"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialConversionFailureRollsBackWithoutCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	a := &PostgresApplier{db: db, schema: "App", spatialCaps: &target.PGSpatialCapabilities{Schema: "Spatial"}, spatialTables: map[string]bool{"Shapes": true}}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`LOCK TABLE "App"."Shapes" IN ROW EXCLUSIVE MODE`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(targetBusinessTriggersSQL)).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"table", "trigger"}))
	mock.ExpectQuery(`SELECT tn.nspname,t.typname,a.atttypmod`).WithArgs("App", "Shapes", "Shape").WillReturnRows(sqlmock.NewRows([]string{"schema", "type", "typmod"}).AddRow("Spatial", "geometry", -1))
	mock.ExpectQuery(`SELECT pg_get_constraintdef`).WithArgs("App", "Shapes", "Shape").WillReturnRows(sqlmock.NewRows([]string{"check"}))
	mock.ExpectRollback()
	_, err = a.Apply(context.Background(), []Change{{Action: "insert", Table: &TableInfo{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"POINT"}}, After: []any{[]byte{0, 0, 0, 0}}}}, Position{File: "bin.1", Pos: 100})
	require.ErrorContains(t, err, "Shapes")
	require.ErrorContains(t, err, "Shape")
	require.ErrorContains(t, err, "截断")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialDriftRollsBackAfterTableLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	a := &PostgresApplier{db: db, schema: "App", spatialCaps: &target.PGSpatialCapabilities{Schema: "Spatial"}, spatialTables: map[string]bool{"Shapes": true}}
	table := &TableInfo{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"POINT"}}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`LOCK TABLE "App"."Shapes" IN ROW EXCLUSIVE MODE`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(targetBusinessTriggersSQL)).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"table", "trigger"}))
	mock.ExpectQuery(`SELECT tn.nspname,t.typname,a.atttypmod`).WithArgs("App", "Shapes", "Shape").WillReturnRows(sqlmock.NewRows([]string{"schema", "type", "typmod"}).AddRow("pg_catalog", "text", -1))
	mock.ExpectRollback()
	_, err = a.Apply(context.Background(), []Change{{Action: "insert", Table: table, After: []any{nil}}, {Action: "insert", Table: table, After: []any{nil}}}, Position{File: "bin.1", Pos: 100})
	require.ErrorContains(t, err, "结构发生变化")
	require.ErrorContains(t, err, "pg_catalog.text")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialTransactionChecksEachTableOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	a := &PostgresApplier{db: db, schema: "App", spatialCaps: &target.PGSpatialCapabilities{Schema: "Spatial"}}
	table := &TableInfo{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"POINT"}}
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectQuery(`SELECT tn.nspname,t.typname,a.atttypmod`).WithArgs("App", "Shapes", "Shape").WillReturnRows(sqlmock.NewRows([]string{"schema", "type", "typmod"}).AddRow("Spatial", "geometry", -1))
	mock.ExpectQuery(`SELECT pg_get_constraintdef`).WithArgs("App", "Shapes", "Shape").WillReturnRows(sqlmock.NewRows([]string{"check"}))
	require.NoError(t, a.checkTransactionSpatialColumns(context.Background(), tx, []Change{{Table: table}, {Table: table}}))
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialSubtypeInsertAndFullRowMatch(t *testing.T) {
	db, mock := newTargetMetadataMock(t)
	a := &PostgresApplier{db: db, schema: "App", spatialCaps: &target.PGSpatialCapabilities{Schema: `Sp"atial`}}
	table := &TableInfo{Name: "Shapes", Columns: []string{"Shape"}, ColumnTypes: []string{"POINT"}, LocatorStrategy: LocatorFullRow}
	raw, err := hex.DecodeString("e61000000101000000000000000000f03f0000000000000040")
	require.NoError(t, err)
	const encoded = "0101000020e6100000000000000000f03f0000000000000040"
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`INSERT INTO "App"."Shapes" ("Shape") VALUES ($1)`).WithArgs(encoded).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, a.insert(context.Background(), tx, table, []any{raw}))
	mock.ExpectExec(`UPDATE "App"."Shapes" SET "Shape"=$1 WHERE ctid=(SELECT ctid FROM "App"."Shapes" WHERE "Sp""atial".ST_AsEWKB("Shape",'NDR') IS NOT DISTINCT FROM "Sp""atial".ST_AsEWKB($2::"Sp""atial".geometry,'NDR') LIMIT 1)`).WithArgs(encoded, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, a.updateFullRow(context.Background(), tx, table, []any{nil}, []any{raw}))
	mock.ExpectExec(`DELETE FROM "App"."Shapes" WHERE ctid=(SELECT ctid FROM "App"."Shapes" WHERE "Sp""atial".ST_AsEWKB("Shape",'NDR') IS NOT DISTINCT FROM "Sp""atial".ST_AsEWKB($1::"Sp""atial".geometry,'NDR') LIMIT 1)`).WithArgs(encoded).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, a.deleteFullRow(context.Background(), tx, table, []any{raw}))
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCDCSpatialLoadsColumnSRIDRestrictions(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	tables := []TableInfo{{Name: "Shapes", Columns: []string{"Id", "Fixed", "Mixed"}, ColumnTypes: []string{"INT", "POINT", "GEOMETRY"}}, {Name: "Ordinary", Columns: []string{"Id"}, ColumnTypes: []string{"INT"}}}
	mock.ExpectQuery(`SELECT COLUMN_NAME, SRS_ID`).WithArgs("Source", "Shapes").WillReturnRows(sqlmock.NewRows([]string{"name", "srid"}).AddRow("Id", nil).AddRow("Fixed", 4326).AddRow("Mixed", nil))
	require.NoError(t, loadCDCSpatialSRIDs(context.Background(), db, "Source", tables))
	require.Len(t, tables[0].ColumnSpatialSRIDs, 3)
	require.Equal(t, uint32(4326), *tables[0].ColumnSpatialSRIDs[1])
	require.Nil(t, tables[0].ColumnSpatialSRIDs[2])
	require.NoError(t, mock.ExpectationsWereMet())
}
