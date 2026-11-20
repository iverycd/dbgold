package source

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengDSNPreservesOptions(t *testing.T) {
	raw := "user:pass@tcp(localhost:3306)/MixedDb?parseTime=true&charset=utf8mb4&loc=Asia%2FShanghai&timeout=3s"
	want, err := mysql.ParseDSN(raw)
	require.NoError(t, err)
	dsn, err := mysqlDamengDSN(raw)
	require.NoError(t, err)
	got, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	want.ParseTime = false
	require.Equal(t, want, got)
	_, err = mysqlDamengDSN("invalid dsn")
	require.Error(t, err)
}

func TestMySQLDamengReadPreservesZeroDates(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "Src"}
	cols := []*sqlmock.Column{
		sqlmock.NewColumn("At").OfType("DATETIME", []byte{}),
		sqlmock.NewColumn("Text").OfType("VARCHAR", []byte{}),
	}
	mock.ExpectQuery("SELECT \\* FROM `Mixed``Table` LIMIT 0, 10").WillReturnRows(
		sqlmock.NewRowsWithColumnDefinition(cols...).
			AddRow([]byte("0000-00-00 00:00:00"), []byte("0000-00-00")).
			AddRow([]byte("2024-00-01 12:34:56.123456"), []byte("unchanged")).
			AddRow([]byte("2024-02-29 12:34:56.123456"), nil))
	columns, types, rows, err := r.ReadPage(context.Background(), "Mixed`Table", nil, 0, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"At", "Text"}, columns)
	require.Equal(t, []string{"DATETIME", "VARCHAR"}, types)
	require.Equal(t, [][]interface{}{
		{"0000-00-00 00:00:00", "0000-00-00"},
		{"2024-00-01 12:34:56.123456", "unchanged"},
		{"2024-02-29 12:34:56.123456", nil},
	}, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMySQLViewsReadRawForNativeAndDamengTargets(t *testing.T) {
	const body = "select to_days(now()), '%Y', convert(abc using utf8mb4) from `Mixed.Db`.`MyTable`"
	for _, dst := range []string{"mysql", "dameng", ""} {
		t.Run(dst, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery(regexp.QuoteMeta(mysqlOscarViewsQuery)).WithArgs("Mixed.Db").WillReturnRows(
				sqlmock.NewRows([]string{"TABLE_NAME", "VIEW_DEFINITION"}).AddRow("Hidden", nil).AddRow("MyView", body))
			r := &MySQLReader{db: db, dbName: "Mixed.Db"}
			views, err := r.GetViewsForTarget(t.Context(), dst)
			require.NoError(t, err)
			require.Len(t, views, 2)
			require.Contains(t, views[0].Unsupported, "SHOW VIEW")
			require.Equal(t, "MyView", views[1].ViewName)
			require.Equal(t, body, views[1].Definition)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
