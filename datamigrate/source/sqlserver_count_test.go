package source

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSQLServerCountRowsUsesExactSchemaAndQuotedNames(t *testing.T) {
	for _, tc := range []struct {
		schema, table, query string
		count                int64
	}{
		{"dbo", "Orders", "SELECT COUNT_BIG(*) FROM [dbo].[Orders]", 0},
		{"Archive", "MiXeD", "SELECT COUNT_BIG(*) FROM [Archive].[MiXeD]", 12},
		{"Own]er", "Tab]le.With.Dot", "SELECT COUNT_BIG(*) FROM [Own]]er].[Tab]]le.With.Dot]", 2147483648},
		{"Own'er", "T]; SELECT 1;--", "SELECT COUNT_BIG(*) FROM [Own'er].[T]]; SELECT 1;--]", 3},
	} {
		t.Run(tc.schema+"/"+tc.table, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			mock.ExpectQuery(`SELECT s.name,t.object_id.*WHERE t.name=@p1 ORDER BY s.name`).WithArgs(tc.table).
				WillReturnRows(sqlmock.NewRows([]string{"schema", "id"}).AddRow(tc.schema, 42)).RowsWillBeClosed()
			mock.ExpectQuery(regexp.QuoteMeta(tc.query)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.count))
			reader := &SQLServerReader{db: db, dbName: "DatabaseNotSchema"}
			count, err := reader.CountRows(context.Background(), tc.table)
			require.NoError(t, err)
			require.Equal(t, tc.count, count)
		})
	}
}

func TestSQLServerCountRowsRejectsMissingOrAmbiguousIdentityBeforeCounting(t *testing.T) {
	for _, scenario := range []string{"missing", "ambiguous"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			rows := sqlmock.NewRows([]string{"schema", "id"})
			if scenario == "ambiguous" {
				rows.AddRow("dbo", 42).AddRow("Archive", 84)
			}
			mock.ExpectQuery(`SELECT s.name,t.object_id.*WHERE t.name=@p1 ORDER BY s.name`).WithArgs("Orders").WillReturnRows(rows).RowsWillBeClosed()
			count, err := (&SQLServerReader{db: db}).CountRows(context.Background(), "Orders")
			require.ErrorContains(t, err, "schema 身份不唯一或不存在")
			require.Zero(t, count)
		})
	}
}

func TestSQLServerCountRowsPropagatesCatalogAndCountFailures(t *testing.T) {
	for _, scenario := range []string{"query", "rows", "scan", "count", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			failure := errors.New("catalog access denied")
			catalog := mock.ExpectQuery(`SELECT s.name,t.object_id`).WithArgs("Orders")
			switch scenario {
			case "query":
				catalog.WillReturnError(failure)
			case "cancelled":
				failure = context.Canceled
				catalog.WillReturnError(failure)
			case "rows":
				catalog.WillReturnRows(sqlmock.NewRows([]string{"schema", "id"}).AddRow("Archive", 42).RowError(0, failure)).RowsWillBeClosed()
			case "scan":
				catalog.WillReturnRows(sqlmock.NewRows([]string{"schema", "id"}).AddRow(nil, 42)).RowsWillBeClosed()
			case "count":
				catalog.WillReturnRows(sqlmock.NewRows([]string{"schema", "id"}).AddRow("Archive", 42)).RowsWillBeClosed()
				mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT_BIG(*) FROM [Archive].[Orders]")).WillReturnError(failure)
			}
			_, err := (&SQLServerReader{db: db}).CountRows(context.Background(), "Orders")
			require.Error(t, err)
			if scenario != "scan" {
				require.ErrorIs(t, err, failure)
			}
		})
	}
}
