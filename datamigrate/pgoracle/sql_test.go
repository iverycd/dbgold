package pgoracle

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func viewColumns() []source.ColumnInfo {
	return []source.ColumnInfo{
		{Name: "id", DataType: "int4"},
		{Name: "enabled", DataType: "bool", IsNullable: true},
		{Name: "name", DataType: "varchar", Length: 100},
		{Name: "code", DataType: "bpchar", Length: 8},
		{Name: "amount", DataType: "numeric", Precision: 12, Scale: 2},
		{Name: "body", DataType: "text"},
		{Name: "at", DataType: "timestamptz", DateTimePrecision: 6},
	}
}

func compileView(t *testing.T, sql string, columns []source.ColumnInfo) (string, []string) {
	t.Helper()
	actual, deps, err := TranslateView(source.ViewInfo{ViewName: "v", Definition: sql, Columns: columns}, "public", "APP", map[string][]source.ColumnInfo{"orders": viewColumns(), "items": viewColumns(), "MixedTable": {{Name: "MixedColumn", DataType: "int8"}}}, strings.ToUpper)
	require.NoError(t, err)
	require.NotContains(t, actual, "`")
	return actual, deps
}

func TestOracleViewBooleanValuesAndConditions(t *testing.T) {
	actual, deps := compileView(t, `SELECT o.enabled, o.id > 0 AS positive, NOT o.enabled AS disabled FROM public.orders o WHERE o.enabled AND (o.id > 0 OR o.enabled IS NULL)`, []source.ColumnInfo{{Name: "enabled", DataType: "bool"}, {Name: "positive", DataType: "bool"}, {Name: "disabled", DataType: "bool"}})
	require.Equal(t, []string{"orders"}, deps)
	require.Contains(t, actual, `"O"."ENABLED" AS "ENABLED"`)
	require.Contains(t, actual, `CASE WHEN ("O"."ID" > 0) THEN 1 WHEN NOT (("O"."ID" > 0)) THEN 0 ELSE NULL END AS "POSITIVE"`)
	require.Contains(t, actual, `WHERE (("O"."ENABLED" = 1) AND`)
	require.Contains(t, actual, `"APP"."ORDERS" "O"`)
	require.NotContains(t, actual, `"ORDERS" AS`)
}

func TestOracleViewJoinsGroupingAndTypedFunctions(t *testing.T) {
	actual, deps := compileView(t, `SELECT o.name, COUNT(*) AS n, SUM(i.amount) AS total, AVG(i.amount) AS mean, MIN(i.amount) AS minimum, MAX(i.amount) AS maximum, COALESCE(SUM(i.amount), 0) AS safe_total FROM (public.orders o LEFT JOIN public.items i ON o.id = i.id) WHERE o.enabled IS NOT FALSE GROUP BY o.name HAVING COUNT(*) > 1`, []source.ColumnInfo{{Name: "name", DataType: "varchar", Length: 100}, {Name: "n", DataType: "int8"}, {Name: "total", DataType: "numeric"}, {Name: "mean", DataType: "numeric"}, {Name: "minimum", DataType: "numeric"}, {Name: "maximum", DataType: "numeric"}, {Name: "safe_total", DataType: "numeric"}})
	require.Equal(t, []string{"items", "orders"}, deps)
	require.Contains(t, actual, `LEFT JOIN "APP"."ITEMS" "I" ON ("O"."ID" = "I"."ID")`)
	require.Contains(t, actual, `WHERE ("O"."ENABLED" <> 0 OR "O"."ENABLED" IS NULL)`)
	require.Contains(t, actual, `GROUP BY "O"."NAME" HAVING (COUNT(*) > 1)`)
	require.Contains(t, actual, `COALESCE(SUM("I"."AMOUNT"), 0)`)
}

func TestOracleViewCaseNullifAndUnion(t *testing.T) {
	actual, deps := compileView(t, `SELECT DISTINCT CASE WHEN enabled THEN name ELSE 'disabled'::text END AS label, NULLIF(amount, 0) AS amount FROM public.orders UNION ALL SELECT CASE id WHEN 1 THEN name ELSE 'other'::text END, amount FROM public.items`, []source.ColumnInfo{{Name: "label", DataType: "varchar", Length: 100}, {Name: "amount", DataType: "numeric"}})
	require.Equal(t, []string{"items", "orders"}, deps)
	require.Contains(t, actual, `SELECT DISTINCT CASE WHEN ("ORDERS"."ENABLED" = 1) THEN "ORDERS"."NAME" ELSE 'disabled' END AS "LABEL"`)
	require.Contains(t, actual, `NULLIF("ORDERS"."AMOUNT", 0)`)
	require.Contains(t, actual, `UNION ALL SELECT CASE WHEN ("ITEMS"."ID" = 1)`)
}

func TestOracleViewOriginalNamesAndCharacterCasts(t *testing.T) {
	actual, deps := compileView(t, `SELECT m."MixedColumn" AS "Result" FROM public."MixedTable" m`, []source.ColumnInfo{{Name: "Result", DataType: "int8"}})
	require.Equal(t, []string{"MixedTable"}, deps)
	require.Contains(t, actual, `"M"."MIXEDCOLUMN" AS "RESULT"`)
	actual, _ = compileView(t, `SELECT code::text FROM public.orders`, []source.ColumnInfo{{Name: "code", DataType: "text"}})
	require.Contains(t, actual, `RTRIM("ORDERS"."CODE")`)
	actual, _ = compileView(t, `SELECT NULL::text, -32768::int2, true`, []source.ColumnInfo{{Name: "empty", DataType: "text"}, {Name: "minimum", DataType: "int2"}, {Name: "enabled", DataType: "bool"}})
	require.Contains(t, actual, `FROM DUAL`)
	require.Contains(t, actual, `CAST(NULL AS CLOB) AS "EMPTY"`)
	require.Contains(t, actual, `1 AS "ENABLED"`)
}

func TestOracleViewRejectsUnsupportedOrChangedSemantics(t *testing.T) {
	for _, tc := range []struct{ sql, typ, reason string }{
		{`WITH x AS (SELECT id FROM orders) SELECT id FROM x`, "int4", "普通 SELECT"},
		{`SELECT row_number() OVER () FROM orders`, "int8", "函数"},
		{`SELECT id FROM orders ORDER BY id`, "int4", "ORDER"},
		{`SELECT DISTINCT ON (id) id FROM orders`, "int4", "DISTINCT ON"},
		{`SELECT body FROM orders WHERE body = 'x'`, "text", "LOB"},
		{`SELECT DISTINCT body FROM orders`, "text", "LOB"},
		{`SELECT body FROM orders UNION ALL SELECT body FROM items`, "text", "LOB"},
		{`SELECT MIN(body) FROM orders`, "text", "LOB"},
		{`SELECT COUNT(*) FROM orders GROUP BY body`, "int8", "LOB"},
		{`SELECT id FROM orders GROUP BY 1`, "int4", "位置编号"},
		{`SELECT COALESCE(body, 'x') FROM orders`, "text", "LOB"},
		{`SELECT id FROM orders WHERE name = ''`, "int4", "空字符串"},
		{`SELECT id FROM orders WHERE id`, "int4", "布尔"},
		{`SELECT COALESCE(name, 1) FROM orders`, "varchar", "隐式混合"},
		{`SELECT SUM(name) FROM orders`, "numeric", "数值"},
		{`SELECT COUNT(SUM(id)) FROM orders`, "int8", "嵌套聚合"},
		{`SELECT id FROM orders WHERE COUNT(*) > 0`, "int4", "聚合"},
		{`SELECT now() FROM orders`, "timestamp", "函数"},
		{`SELECT id FROM other.orders`, "int4", "跨 Schema"},
		{`SELECT id FROM missing`, "int4", "已知源对象"},
		{`SELECT id FROM orders a JOIN items b ON a.id=b.id`, "int4", "歧义"},
		{`SELECT o.id FROM orders o; DROP TABLE x`, "int4", "FROM"},
		{`SELECT 1.9::int4 FROM orders`, "int4", "舍入"},
		{`SELECT 32768::int2 FROM orders`, "int2", "范围"},
		{`SELECT amount::int4 FROM orders`, "int4", "精度"},
		{`SELECT NULLIF(NULL, 1) FROM orders`, "numeric", "NULLIF"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			_, _, err := TranslateView(source.ViewInfo{Definition: tc.sql, Columns: []source.ColumnInfo{{Name: "v", DataType: tc.typ, Length: 100}}}, "public", "APP", map[string][]source.ColumnInfo{"orders": viewColumns(), "items": viewColumns()}, strings.ToUpper)
			require.ErrorContains(t, err, tc.reason)
		})
	}
}

func TestOracleChecksPreserveBooleanThreeValueLogic(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`CHECK (enabled)`, `("ENABLED" = 1)`},
		{`NOT enabled`, `(NOT ("ENABLED" = 1))`},
		{`enabled IS NOT TRUE`, `("ENABLED" <> 1 OR "ENABLED" IS NULL)`},
		{`enabled IS TRUE`, `("ENABLED" = 1 AND "ENABLED" IS NOT NULL)`},
		{`enabled IS FALSE`, `("ENABLED" = 0 AND "ENABLED" IS NOT NULL)`},
		{`CHECK (amount > 0 AND (enabled OR enabled IS NULL))`, `(("AMOUNT" > 0) AND (("ENABLED" = 1) OR ("ENABLED" IS NULL)))`},
		{`(id > 0) = true`, `(CASE WHEN ("ID" > 0) THEN 1 WHEN NOT (("ID" > 0)) THEN 0 ELSE NULL END = 1)`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			actual, err := CheckExpression(tc.input, viewColumns(), strings.ToUpper)
			require.NoError(t, err)
			require.Equal(t, tc.want, actual)
		})
	}
	for _, raw := range []string{`CHECK (body = 'x')`, `CHECK (name = '')`, `CHECK (id)`, `CHECK (missing > 0)`, `CHECK (orders.id > 0)`, `CHECK (SUM(id) > 0)`, `CHECK (id > 0) NOT VALID`, `CHECK (id > 0); DROP TABLE orders`, `CHECK (id > '1')`} {
		_, err := CheckExpression(raw, viewColumns(), strings.ToUpper)
		require.Error(t, err, raw)
	}
}
