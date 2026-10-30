package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Current rows are intentionally safe. Preflight must prove the future input
// domain as well, before creating any target table or copying those rows.
func TestMySQLDivisionMinimumPartitionPreflightIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" {
		t.Skip("需要独立 MySQL/PG 测试库及明确 DDL 标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("postgres", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	var database, version string
	require.NoError(t, src.QueryRowContext(ctx, "SELECT DATABASE(),VERSION()").Scan(&database, &version))
	t.Logf("source MySQL %s", version)
	name := fmt.Sprintf("dbgold_div_min_%d", time.Now().UnixNano())
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+q(name))
	require.NoError(t, err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, err := dst.ExecContext(cleanup, "DROP SCHEMA "+q(name)+" CASCADE")
		require.NoError(t, err)
	}()
	_, err = src.ExecContext(ctx, "CREATE TABLE `"+name+"` (`k` BIGINT NOT NULL) PARTITION BY HASH (`k` DIV 1) PARTITIONS 3")
	require.NoError(t, err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, err := src.ExecContext(cleanup, "DROP TABLE `"+name+"`")
		require.NoError(t, err)
	}()
	_, err = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (1)")
	require.NoError(t, err)
	r, err := source.NewMySQL(srcDSN, database, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewPostgres(dstDSN, name, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	for _, content := range []string{"both", "schema_only"} {
		t.Run(content, func(t *testing.T) {
			cfg := Config{Mode: "include", Filter: name, Content: content, TargetDBType: "postgres", TargetSchema: name, PageSize: 2, MaxParallel: 1, ValidateData: true}
			job := &Job{LogCh: make(chan string, 8192)}
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			require.True(t, report.HasFailures(), "unsafe future DIV inputs must block despite the current safe row")
			require.NotNil(t, report.PartitionMigration)
			require.Len(t, report.PartitionMigration.Tables, 1)
			require.Contains(t, fmt.Sprintf("%+v", report.PartitionMigration.Tables[0]), "DIV 负商可能达到 BIGINT 最小值")
			var relations int
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1`, name).Scan(&relations))
			require.Zero(t, relations, "preflight rejection must precede creating root or leaf tables")
		})
	}
	_, err = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-9223372036854775808)")
	var overflow *mysql.MySQLError
	require.ErrorAs(t, err, &overflow)
	require.Equal(t, uint16(1690), overflow.Number, "the native partition expression rejects the future minimum input")
	var rows int
	var key int64
	require.NoError(t, src.QueryRowContext(ctx, "SELECT COUNT(*),MIN(k) FROM `"+name+"`").Scan(&rows, &key))
	require.Equal(t, 1, rows)
	require.Equal(t, int64(1), key)
}
