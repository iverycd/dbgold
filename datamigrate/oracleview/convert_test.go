package oracleview

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTranslateOracleViews(t *testing.T) {
	sql, deps, err := Convert("SELECT a.`Id`, IFNULL(a.`Name`, 'KeepMiXeD'), IF(a.n > 0, 'yes', 'no') AS flag, NOW() AS t FROM `src`.`Orders` a LEFT JOIN `src`.`Items` b ON a.Id=b.Id WHERE a.n > 1 GROUP BY a.Id, a.Name,a.n HAVING COUNT(*) > 0 ORDER BY a.Id DESC", "src", "APP", nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"Orders", "Items"}, deps)
	for _, part := range []string{`"APP"."ORDERS" "A"`, `"A"."ID"`, `'KeepMiXeD'`, `COALESCE(`, `CASE WHEN`, `CURRENT_TIMESTAMP`, `LEFT JOIN`, `HAVING (COUNT(1) > 0)`, `DESC NULLS LAST`} {
		require.Contains(t, sql, part)
	}
	sql, _, err = Convert("SELECT id FROM a UNION ALL SELECT id FROM b", "src", "APP", nil)
	require.NoError(t, err)
	require.Contains(t, sql, `UNION ALL SELECT "ID" FROM "APP"."B"`)
	sql, _, err = Convert("SELECT 1 AS a", "src", "APP", nil)
	require.NoError(t, err)
	require.Equal(t, `SELECT 1 AS "A" FROM DUAL`, sql)
	sql, _, err = Convert("SELECT (id>1) AS yes FROM a WHERE id", "src", "APP", nil)
	require.NoError(t, err)
	require.Contains(t, sql, "CASE WHEN")
	require.Contains(t, sql, `WHERE ("ID" <> 0)`)
	sql, _, err = Convert("SELECT id FROM a WHERE id IN (SELECT id FROM b)", "src", "APP", nil)
	require.NoError(t, err)
	require.Contains(t, sql, `IN (SELECT "ID" FROM "APP"."B")`)
}
func TestOracleUnsupportedViews(t *testing.T) {
	for _, sql := range []string{"SELECT * FROM a LIMIT 1", "SELECT GROUP_CONCAT(x) FROM a", "SELECT JSON_EXTRACT(x,'$.a') FROM a", "SELECT id FROM other.a", "SELECT a <=> b FROM t", "SELECT ROW_NUMBER() OVER () FROM t", "DELETE FROM t", "SELECT IFNULL(a,b) FROM t; DROP TABLE t"} {
		_, _, err := Convert(sql, "src", "APP", nil)
		require.Error(t, err, sql)
	}
	sql, _, err := Convert("SELECT id FROM other.a WHERE note='other.a'", "src", "APP", []string{"other"})
	require.NoError(t, err)
	require.Contains(t, sql, `"APP"."A"`)
	require.Contains(t, sql, `'other.a'`)
}
