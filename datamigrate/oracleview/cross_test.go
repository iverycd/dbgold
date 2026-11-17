package oracleview

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCrossOracleViews(t *testing.T) {
	for _, tc := range []struct{ source, schema, sql string }{
		{"sqlserver", "dbo", `CREATE VIEW [dbo].[MixedView] AS SELECT o.[Id], COALESCE(o.[Name], N'Case[Keep]') AS [Label] FROM [dbo].[Orders] o WHERE o.[Id]>0 UNION ALL SELECT i.[Id], i.[Name] FROM [dbo].[Items] i`},
		{"dameng", "APP", `SELECT o."Id", COUNT(i."Id") AS "N" FROM "APP"."Orders" o LEFT JOIN "APP"."Items" i ON o."Id"=i."Id" GROUP BY o."Id"`},
	} {
		got, deps, e := ConvertCross(tc.sql, tc.source, tc.schema, "TARGET")
		require.NoError(t, e)
		require.Contains(t, got, `"TARGET"."ORDERS"`)
		require.ElementsMatch(t, []string{"Orders", "Items"}, deps)
	}
	for _, sql := range []string{`SELECT * FROM dbo.T`, `SELECT Id FROM other.T`, `SELECT TOP 1 Id FROM dbo.T`, `SELECT Id FROM dbo.T; DELETE FROM dbo.T`, `SELECT unknown_function(Id) FROM dbo.T`} {
		_, _, e := ConvertCross(sql, "sqlserver", "dbo", "APP")
		require.Error(t, e, sql)
	}
}
