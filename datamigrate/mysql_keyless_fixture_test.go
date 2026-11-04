package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedMySQLKeylessFixture creates the only business table in a fresh schema.
// The sequence used to generate deterministic data is never a table column.
func seedMySQLKeylessFixture(t *testing.T, db *sql.DB) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	var originalGIPK, originalShowGIPK int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_generate_invisible_primary_key, @@SESSION.show_gipk_in_create_table_and_information_schema").Scan(&originalGIPK, &originalShowGIPK))
	_, err = conn.ExecContext(ctx, "SET SESSION sql_generate_invisible_primary_key=OFF, SESSION show_gipk_in_create_table_and_information_schema=ON")
	require.NoError(t, err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, err := conn.ExecContext(cleanup, fmt.Sprintf("SET SESSION sql_generate_invisible_primary_key=%d, SESSION show_gipk_in_create_table_and_information_schema=%d", originalGIPK, originalShowGIPK))
		require.NoError(t, err)
	}()

	_, err = conn.ExecContext(ctx, "CREATE TABLE scan_keyless (n INT NULL, tag VARCHAR(32) NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	require.NoError(t, err)

	// Six decimal digits enumerate [0, 999999] in one INSERT without recursion,
	// a persistent sequence table, or per-row client/server round trips.
	_, err = conn.ExecContext(ctx, `INSERT INTO scan_keyless(n,tag)
WITH digits(d) AS (
	SELECT 0 UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4
	UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9
), numbers(i) AS (
	SELECT u.d+10*t.d+100*h.d+1000*a.d+10000*b.d+100000*c.d FROM digits u
	CROSS JOIN digits t CROSS JOIN digits h CROSS JOIN digits a CROSS JOIN digits b CROSS JOIN digits c
)
SELECT CASE MOD(i,10) WHEN 0 THEN NULL WHEN 1 THEN -1 WHEN 2 THEN NULL ELSE i END,
       CASE MOD(i,10) WHEN 0 THEN NULL WHEN 1 THEN 'duplicate' WHEN 2 THEN CONCAT('partial-',i DIV 10)
            WHEN 3 THEN NULL ELSE CONCAT('row-',i) END
FROM numbers`)
	require.NoError(t, err)
	var table, ddl string
	require.NoError(t, conn.QueryRowContext(ctx, "SHOW CREATE TABLE scan_keyless").Scan(&table, &ddl))
	require.Equal(t, "scan_keyless", table)
	require.Contains(t, strings.ToUpper(ddl), "ENGINE=INNODB")
	require.NotContains(t, strings.ToUpper(ddl), "PRIMARY KEY")
	require.NotContains(t, strings.ToUpper(ddl), "AUTO_INCREMENT")
	indexes, err := conn.QueryContext(ctx, "SHOW INDEX FROM scan_keyless")
	require.NoError(t, err)
	hasIndex := indexes.Next()
	closeErr := indexes.Close()
	require.NoError(t, indexes.Err())
	require.NoError(t, closeErr)
	require.False(t, hasIndex, "the benchmark table must have no indexes")
	var tables, columns, nullable int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()").Scan(&tables))
	require.Equal(t, 1, tables)
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*), SUM(IS_NULLABLE='YES' AND COLUMN_NAME IN ('n','tag')) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='scan_keyless'").Scan(&columns, &nullable))
	require.Equal(t, []int{2, 2}, []int{columns, nullable})

	var total, allNull, duplicates, nullNumberOnly, nullTagOnly, distinctSample int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*),
		COUNT(CASE WHEN n IS NULL AND tag IS NULL THEN 1 END),
		COUNT(CASE WHEN n=-1 AND tag='duplicate' THEN 1 END),
		COUNT(CASE WHEN n IS NULL AND tag IS NOT NULL THEN 1 END),
		COUNT(CASE WHEN n IS NOT NULL AND tag IS NULL THEN 1 END),
		COUNT(CASE WHEN n=999999 AND tag='row-999999' THEN 1 END)
		FROM scan_keyless`).Scan(&total, &allNull, &duplicates, &nullNumberOnly, &nullTagOnly, &distinctSample))
	require.Equal(t, []int{1000000, 100000, 100000, 100000, 100000, 1}, []int{total, allNull, duplicates, nullNumberOnly, nullTagOnly, distinctSample})
	t.Logf("scan_keyless: %s; indexes=0 tables=%d columns=%d nullable=%d rows=%d all-NULL=%d duplicate(-1,duplicate)=%d partial-NULL=%d/%d singleton(999999,row-999999)=%d", ddl, tables, columns, nullable, total, allNull, duplicates, nullNumberOnly, nullTagOnly, distinctSample)
	return []string{table}
}
