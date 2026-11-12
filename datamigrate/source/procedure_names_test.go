package source

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestListProcedureNamesUsesSourceScopeAndPreservesSpelling(t *testing.T) {
	type reader interface {
		ListProcedureNames(context.Context) ([]string, error)
	}
	tests := []struct {
		name string
		open func(*sql.DB) reader
		sql  string
		args []driver.Value
	}{
		{
			name: "mysql",
			open: func(db *sql.DB) reader { return &MySQLReader{db: db, dbName: "MixedDB"} },
			sql:  `SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE' ORDER BY ROUTINE_NAME`,
			args: []driver.Value{"MixedDB"},
		},
		{
			name: "sqlserver",
			open: func(db *sql.DB) reader { return &SQLServerReader{db: db, dbName: "MixedDB"} },
			sql:  `SELECT name FROM sys.procedures WHERE is_ms_shipped = 0 ORDER BY name`,
		},
		{
			name: "oracle",
			open: func(db *sql.DB) reader { return &OracleReader{db: db, owner: "ORACLE_OWNER"} },
			sql:  `SELECT OBJECT_NAME FROM ALL_OBJECTS WHERE OWNER = :1 AND OBJECT_TYPE = 'PROCEDURE' ORDER BY OBJECT_NAME`,
			args: []driver.Value{"ORACLE_OWNER"},
		},
		{
			name: "dameng",
			open: func(db *sql.DB) reader { return &DaMengReader{db: db, schema: "MixedOwner"} },
			sql:  `SELECT OBJECT_NAME FROM ALL_OBJECTS WHERE OWNER = ? AND OBJECT_TYPE = 'PROCEDURE' ORDER BY OBJECT_NAME`,
			args: []driver.Value{"MixedOwner"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			mock.ExpectQuery("^" + regexp.QuoteMeta(test.sql) + "$").WithArgs(test.args...).
				WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("ChangeDataToMain").AddRow("CREATEPVI")).RowsWillBeClosed()
			names, err := test.open(db).ListProcedureNames(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{"ChangeDataToMain", "CREATEPVI"}, names)
		})
	}
}

func TestListProcedureNamesRejectsIncompleteInventory(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	catalogErr := errors.New("source catalog read failed")
	mock.ExpectQuery(`SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES`).WithArgs("app").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("ChangeDataToMain").AddRow("CreatePVI").RowError(1, catalogErr)).RowsWillBeClosed()
	names, err := (&MySQLReader{db: db, dbName: "app"}).ListProcedureNames(context.Background())
	require.ErrorIs(t, err, catalogErr)
	require.Nil(t, names)
}

func TestMySQLListProcedureNamesUsesSnapshotConnection(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	mock.ExpectQuery(`SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES`).WithArgs("app").
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectRollback()
	names, err := (&MySQLReader{db: db, dbName: "app", snapshotTx: tx}).ListProcedureNames(ctx)
	require.NoError(t, err)
	require.Empty(t, names)
}
