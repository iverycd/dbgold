package source

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleOscarViewsPreserveNamesAndLiterals(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &OracleReader{db: db, owner: "APP"}
	mock.ExpectQuery(`SELECT VIEW_NAME, TEXT FROM ALL_VIEWS`).WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"VIEW_NAME", "TEXT"}).AddRow("MixedView", `SELECT "MixedColumn", 'KeepMiXeD' AS label FROM "MixedTable"`))
	views, err := r.GetViewsForTarget(context.Background(), "oscar")
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, "MixedView", views[0].ViewName)
	require.Contains(t, views[0].Definition, `'KeepMiXeD'`)
	require.Contains(t, views[0].Definition, `"MixedColumn"`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDaMengOscarViewsPreserveNamesAndLiterals(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &DaMengReader{db: db, schema: "MixedSchema"}
	query := `SELECT VIEW_NAME, DBMS_METADATA.GET_DDL('VIEW', VIEW_NAME, ?) AS view_ddl FROM ALL_VIEWS WHERE OWNER = ? ORDER BY VIEW_NAME`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("MixedSchema", "MixedSchema").WillReturnRows(sqlmock.NewRows([]string{"VIEW_NAME", "view_ddl"}).AddRow("MixedView", `CREATE VIEW "MixedView" AS SELECT "MixedColumn", 'KeepMiXeD' AS label FROM "MixedTable"`))
	views, err := r.GetViewsForTarget(context.Background(), "oscar")
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, "MixedView", views[0].ViewName)
	require.Contains(t, views[0].Definition, `'KeepMiXeD'`)
	require.Contains(t, views[0].Definition, `"MixedColumn"`)
	require.NoError(t, mock.ExpectationsWereMet())
}
