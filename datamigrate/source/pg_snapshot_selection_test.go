package source

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func pgSnapshotIdentityRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"oid", "name", "kind", "inherits", "comment", "readable", "rowSecurity"})
}

func expectSelectedPGSnapshotMetadata(mock sqlmock.Sqlmock, schema, table string) {
	empty := func(pattern string) {
		mock.ExpectQuery(pattern).WithArgs(schema).WillReturnRows(sqlmock.NewRows([]string{"unused"})).RowsWillBeClosed()
	}
	empty(`SELECT t.relname,i.relname,`)
	empty(`SELECT c.relname,a.attnum,a.attname`)
	empty(`SELECT c.conname,t.relname`)
	empty(`SELECT c.relname,a.attnum,a.attname`)
	if table != "" {
		mock.ExpectQuery(`SELECT a.attnum,a.attname,t.typname`).WithArgs(schema, table).WillReturnRows(sqlmock.NewRows([]string{"ordinal", "name", "type", "typeKind", "typeSchema", "mod", "nullable", "default", "identity", "generated", "comment", "formatted"}).AddRow(1, "Id", "int8", "b", "pg_catalog", -1, false, nil, "", "", nil, "bigint")).RowsWillBeClosed()
	}
	empty(`SELECT t.relname,c.conname,c.contype`)
	empty(`SELECT t.relname,i.relname FROM`)
	empty(`SELECT t.relname,g.tgname`)
	empty(`SELECT p.proname,oidvectortypes`)
	empty(`SELECT c.relname,pg_get_viewdef`)
	empty(`SELECT c.oid,n.nspname,c.relname`)
}

func TestPGSnapshotSelectedRootsSkipExcludedPermissionsAndColumns(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("App").WillReturnRows(pgSnapshotIdentityRows().AddRow(1, "BlockedRoot", "r", false, nil, false, false).AddRow(2, "PhysicalChild", "r", true, nil, false, false).AddRow(3, "Good", "r", false, "keep comment", true, false).AddRow(4, "ExcludedBrokenColumns", "r", false, nil, true, false)).RowsWillBeClosed()
	expectSelectedPGSnapshotMetadata(mock, "App", "Good")
	snapshot, err := r.ReadPGSnapshotForTables(context.Background(), []string{"Good"})
	require.NoError(t, err)
	require.Len(t, snapshot.Tables, 1)
	require.Equal(t, "Good", snapshot.Tables[0].Info.TableName)
	require.Equal(t, "keep comment", snapshot.Tables[0].Info.Comment)
	require.Len(t, snapshot.Tables[0].Info.Columns, 1)
}

func TestPGSnapshotOriginalContractStillReportsUnreadableTable(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("App").WillReturnRows(pgSnapshotIdentityRows().AddRow(1, "Unreadable", "r", false, nil, false, false)).RowsWillBeClosed()
	snapshot, err := r.ReadPGSnapshot(context.Background())
	require.Nil(t, snapshot)
	require.ErrorContains(t, err, "Unreadable")
	require.ErrorContains(t, err, "SELECT")
}

func TestPGSnapshotSelectedRootCannotDisappearSilently(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("App").WillReturnRows(pgSnapshotIdentityRows()).RowsWillBeClosed()
	snapshot, err := r.ReadPGSnapshotForTables(context.Background(), []string{"Missing"})
	require.Nil(t, snapshot)
	require.ErrorContains(t, err, "Missing")
	require.ErrorContains(t, err, "身份已变化")
}

func TestPGSnapshotEmptySelectionDoesNotBecomeWholeSchema(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("App").WillReturnRows(pgSnapshotIdentityRows().AddRow(1, "Unreadable", "r", false, nil, false, false)).RowsWillBeClosed()
	expectSelectedPGSnapshotMetadata(mock, "App", "")
	snapshot, err := r.ReadPGSnapshotForTables(context.Background(), []string{})
	require.NoError(t, err)
	require.Empty(t, snapshot.Tables)
}

func TestPGSnapshotSelectedSequencesDoNotReadExcludedRootState(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name"}).AddRow(11, "App", "ExcludedSeq").AddRow(12, "App", "GoodSeq"))
	mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname`).WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column"}).AddRow("App", "Excluded", "Id"))
	mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname`).WithArgs(int64(12)).WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column"}).AddRow("App", "Good", "Id"))
	mock.ExpectQuery(`SELECT last_value,is_called FROM "App"\."GoodSeq"`).WillReturnRows(sqlmock.NewRows([]string{"last", "called"}).AddRow(5, true))
	mock.ExpectQuery(`SELECT increment_by,min_value,max_value,is_cycled FROM "App"\."GoodSeq"`).WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "cycle"}).AddRow(1, 1, 100, false))
	snapshot := &PGSnapshot{Tables: []PGTable{{Info: TableDDLInfo{TableName: "Good"}}}}
	err := r.loadPGSequencesForTables(context.Background(), snapshot, map[string]bool{"Good": true})
	require.NoError(t, err)
	require.Len(t, snapshot.Tables[0].Sequences, 1)
	require.Equal(t, "GoodSeq", snapshot.Tables[0].Sequences[0].Name)
	require.Equal(t, int64(6), snapshot.Tables[0].Sequences[0].StartValue)
}

func TestPGSnapshotSelectedSharedSequenceKeepsAllReferences(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name"}).AddRow(12, "App", "SharedSeq"))
	mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname`).WithArgs(int64(12)).WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column"}).AddRow("App", "Good", "Id").AddRow("App", "Excluded", "Id"))
	mock.ExpectQuery(`SELECT last_value,is_called FROM "App"\."SharedSeq"`).WillReturnRows(sqlmock.NewRows([]string{"last", "called"}).AddRow(5, true))
	mock.ExpectQuery(`SELECT increment_by,min_value,max_value,is_cycled FROM "App"\."SharedSeq"`).WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "cycle"}).AddRow(1, 1, 100, false))
	snapshot := &PGSnapshot{Tables: []PGTable{{Info: TableDDLInfo{TableName: "Good"}}}}
	err := r.loadPGSequencesForTables(context.Background(), snapshot, map[string]bool{"Good": true})
	require.NoError(t, err)
	require.Len(t, snapshot.Tables[0].Sequences, 1)
	require.True(t, snapshot.Tables[0].Sequences[0].Shared)
}
