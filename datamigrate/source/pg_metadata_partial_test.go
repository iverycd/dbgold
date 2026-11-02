package source

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPGMigrationSnapshotRetainsHealthyTables(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("App").WillReturnRows(pgSnapshotIdentityRows().AddRow(1, "Unreadable", "r", false, nil, false, false).AddRow(2, "Good", "r", false, nil, true, false)).RowsWillBeClosed()
	expectSelectedPGSnapshotMetadata(mock, "App", "Good")
	snap, err := r.ReadPGMigrationSnapshot(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, snap.Tables, 2)
	require.NotEmpty(t, snap.Tables[0].Unsupported)
	require.Equal(t, "Unreadable", snap.Tables[0].Info.TableName)
	require.Len(t, snap.Tables[1].Info.Columns, 1)
}
func TestPGMigrationSnapshotStopsOnTaskError(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &PostgresReader{db: db, schema: "App", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT c.oid,c.relname,c.relkind`).WithArgs("App").WillReturnError(driver.ErrBadConn)
	snap, err := r.ReadPGMigrationSnapshot(context.Background(), nil)
	require.Error(t, err)
	require.Nil(t, snap)
}
