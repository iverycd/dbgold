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

func TestRoutineNamesUseSourceScopeAndPreserveSpelling(t *testing.T) {
	type reader interface {
		Reader
		ListProcedureNames(context.Context) ([]string, error)
	}
	tests := []struct {
		name         string
		open         func(*sql.DB) reader
		procedureSQL string
		functionSQL  string
		args         []driver.Value
	}{
		{
			name:         "mysql",
			open:         func(db *sql.DB) reader { return &MySQLReader{db: db, dbName: "MixedDB"} },
			procedureSQL: `SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE' ORDER BY ROUTINE_NAME`,
			functionSQL:  `SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION' ORDER BY ROUTINE_NAME`,
			args:         []driver.Value{"MixedDB"},
		},
		{
			name:         "sqlserver",
			open:         func(db *sql.DB) reader { return &SQLServerReader{db: db, dbName: "MixedDB"} },
			procedureSQL: `SELECT name FROM sys.procedures WHERE is_ms_shipped = 0 ORDER BY name`,
			functionSQL:  `SELECT name FROM sys.objects WHERE type IN ('FN','IF','TF','FS','FT') AND is_ms_shipped = 0 ORDER BY name`,
		},
		{
			name:         "oracle",
			open:         func(db *sql.DB) reader { return &OracleReader{db: db, owner: "ORACLE_OWNER"} },
			procedureSQL: `SELECT OBJECT_NAME FROM ALL_OBJECTS WHERE OWNER = :1 AND OBJECT_TYPE = 'PROCEDURE' ORDER BY OBJECT_NAME`,
			functionSQL:  `SELECT OBJECT_NAME FROM ALL_OBJECTS WHERE OWNER = :1 AND OBJECT_TYPE = 'FUNCTION' ORDER BY OBJECT_NAME`,
			args:         []driver.Value{"ORACLE_OWNER"},
		},
		{
			name:         "dameng",
			open:         func(db *sql.DB) reader { return &DaMengReader{db: db, schema: "MixedOwner"} },
			procedureSQL: `SELECT OBJECT_NAME FROM ALL_OBJECTS WHERE OWNER = ? AND OBJECT_TYPE = 'PROCEDURE' ORDER BY OBJECT_NAME`,
			functionSQL:  `SELECT OBJECT_NAME FROM ALL_OBJECTS WHERE OWNER = ? AND OBJECT_TYPE = 'FUNCTION' ORDER BY OBJECT_NAME`,
			args:         []driver.Value{"MixedOwner"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			mock.ExpectQuery("^" + regexp.QuoteMeta(test.procedureSQL) + "$").WithArgs(test.args...).
				WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("ChangeDataToMain").AddRow("CREATEPVI")).RowsWillBeClosed()
			mock.ExpectQuery("^" + regexp.QuoteMeta(test.functionSQL) + "$").WithArgs(test.args...).
				WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("getChildOuList").AddRow("GETSCORE")).RowsWillBeClosed()
			r := test.open(db)
			ctx := context.Background()
			names, err := r.ListProcedureNames(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"ChangeDataToMain", "CREATEPVI"}, names)
			names, err = ListFunctionNames(ctx, r)
			require.NoError(t, err)
			require.Equal(t, []string{"getChildOuList", "GETSCORE"}, names)
		})
	}
}

func TestRoutineNamesRejectIncompleteInventory(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	catalogErr := errors.New("source catalog read failed")
	mock.ExpectQuery(`SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES .*ROUTINE_TYPE = 'PROCEDURE'`).WithArgs("app").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("ChangeDataToMain").AddRow("CreatePVI").RowError(1, catalogErr)).RowsWillBeClosed()
	mock.ExpectQuery(`SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES .*ROUTINE_TYPE = 'FUNCTION'`).WithArgs("app").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("getChildOuList").AddRow("GETSCORE").RowError(1, catalogErr)).RowsWillBeClosed()
	r := &MySQLReader{db: db, dbName: "app"}
	ctx := context.Background()
	names, err := r.ListProcedureNames(ctx)
	require.ErrorIs(t, err, catalogErr)
	require.Nil(t, names)
	names, err = ListFunctionNames(ctx, r)
	require.ErrorIs(t, err, catalogErr)
	require.Nil(t, names)
}

func TestMySQLRoutineNamesUseSnapshotConnection(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	mock.ExpectQuery(`SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES .*ROUTINE_TYPE = 'PROCEDURE'`).WithArgs("app").
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectQuery(`SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES .*ROUTINE_TYPE = 'FUNCTION'`).WithArgs("app").
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectRollback()
	r := &MySQLReader{db: db, dbName: "app", snapshotTx: tx}
	names, err := r.ListProcedureNames(ctx)
	require.NoError(t, err)
	require.Empty(t, names)
	names, err = ListFunctionNames(ctx, r)
	require.NoError(t, err)
	require.Empty(t, names)
}
