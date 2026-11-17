package sqlserverview

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOracleSQLServerViewIdentifiersAndAggregates(t *testing.T) {
	tables := map[string]*source.TableDDLInfo{
		"ORDERS": {Columns: []source.ColumnInfo{
			{Name: "ID", DataType: "NUMBER", Precision: 12},
			{Name: "Amount]", DataType: "NUMBER", Precision: 12, Scale: 2},
			{Name: "NAME", DataType: "VARCHAR2"},
		}},
		"LineItems": {Columns: []source.ColumnInfo{{Name: "ORDER_ID", DataType: "NUMBER", Precision: 12}}},
	}
	converted, deps, err := ConvertOraclePostgres(`SELECT o.id, SUM(o."Amount]") AS "Total", COUNT(i.order_id) AS Qty
FROM app.orders o LEFT OUTER JOIN "APP"."LineItems" i ON o.id = i.order_id
WHERE o.name IS NOT NULL AND o.id BETWEEN 1 AND 9
GROUP BY o.id HAVING COUNT(*) > 1;`, "oracle", "APP", "Sales]", nil, strings.ToLower, tables)
	require.NoError(t, err)
	require.Equal(t, []string{"LineItems", "ORDERS"}, deps)
	require.Equal(t, `SELECT [o].[id], SUM(CAST([o].[amount]]] AS decimal(38,2))) AS [total], COUNT_BIG([i].[order_id]) AS [qty] FROM [Sales]]].[orders] AS [o] LEFT JOIN [Sales]]].[lineitems] AS [i] ON ([o].[id] = [i].[order_id]) WHERE (([o].[name] IS NOT NULL) AND ([o].[id] BETWEEN 1 AND 9)) GROUP BY [o].[id] HAVING (COUNT_BIG(*) > 1)`, converted)
}

func TestPGSQLServerViewDiscoveryAndPredicates(t *testing.T) {
	body := `SELECT o."ID", p.order_id, s.active
FROM ("Src"."Orders" o FULL JOIN "Src".payments p ON ((o."ID" = p.order_id)))
JOIN "Src".states s ON s.order_id = o."ID"
WHERE NOT s.active OR (s.active IS NOT TRUE AND p.order_id NOT IN (2, 3));`
	depsExpected := []string{"Orders", "payments", "states"}
	converted, deps, err := ConvertOraclePostgres(body, "postgres", "Src", "dbo", nil, nil, nil)
	require.ErrorContains(t, err, "无法确认视图依赖")
	require.Equal(t, depsExpected, deps)
	require.Empty(t, converted)

	tables := map[string]*source.TableDDLInfo{
		"Orders":   {Columns: []source.ColumnInfo{{Name: "ID", DataType: "bigint"}}},
		"payments": {Columns: []source.ColumnInfo{{Name: "order_id", DataType: "bigint"}}},
		"states":   {Columns: []source.ColumnInfo{{Name: "order_id", DataType: "bigint"}, {Name: "active", DataType: "boolean"}}},
	}
	converted, deps, err = ConvertOraclePostgres(body, "gaussdb", "Src", "dbo", nil, nil, tables)
	require.NoError(t, err)
	require.Equal(t, depsExpected, deps)
	require.Contains(t, converted, `FROM ([dbo].[Orders] AS [o] FULL JOIN [dbo].[payments] AS [p] ON ([o].[ID] = [p].[order_id])) INNER JOIN [dbo].[states] AS [s]`)
	require.Contains(t, converted, `((NOT ([s].[active] = 1)) OR ((NOT ([s].[active] = 1) OR [s].[active] IS NULL) AND ([p].[order_id] NOT IN (2, 3))))`)
}

func TestOraclePGSQLServerViewRejectsUnverifiedSyntaxAndSemantics(t *testing.T) {
	tables := map[string]*source.TableDDLInfo{
		"items": {Columns: []source.ColumnInfo{{Name: "id", DataType: "bigint"}, {Name: "name", DataType: "text"}}},
	}
	for _, tc := range []struct{ body, message string }{
		{`SELECT id FROM foreign_schema.items`, "迁移范围之外"},
		{`SELECT id FROM items WHERE name = 'x'`, "排序规则"},
		{`SELECT name FROM items GROUP BY name`, "排序规则"},
		{`SELECT DISTINCT name FROM items`, "DISTINCT"},
		{`SELECT id FROM items WHERE id IN (SELECT id FROM items)`, "标识符"},
		{`SELECT id FROM items ORDER BY id`, "视图子句"},
		{`SELECT id FROM items JOIN items AS next USING (id)`, "需要 ON"},
		{`SELECT lower(name) FROM items`, "人工适配"},
		{`SELECT count(*) FROM items; DROP TABLE items`, "单条"},
		{`SELECT id INTO other FROM items`, "投影语法"},
		{`WITH n AS (SELECT id FROM items) SELECT id FROM n`, "需要 SELECT"},
		{`SELECT missing FROM items`, "无法解析视图源列"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			converted, _, err := ConvertOraclePostgres(tc.body, "postgres", "public", "dbo", []string{"foreign_schema"}, nil, tables)
			require.ErrorContains(t, err, tc.message)
			require.Empty(t, converted)
		})
	}
}

func TestOraclePGSQLServerViewStringLiteralEscaping(t *testing.T) {
	for _, tc := range []struct{ body, source, want string }{
		{`SELECT '' AS empty_string, N'北京 O''Neil; --' AS national_text`, "oracle", `SELECT CAST(NULL AS nvarchar(1)) AS [EMPTY_STRING], N'北京 O''Neil; --' AS [NATIONAL_TEXT]`},
		{`SELECT E' O\'Neil; -- ' AS "Text"`, "postgres", `SELECT N' O''Neil; -- ' AS [Text]`},
	} {
		got, deps, err := ConvertOraclePostgres(tc.body, tc.source, "public", "dbo", nil, nil, nil)
		require.NoError(t, err)
		require.Empty(t, deps)
		require.Equal(t, tc.want, got)
	}
}
