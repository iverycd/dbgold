package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestMySQLHashPostgresPartitionIntegration(t *testing.T) {
	for _, method := range []string{"HASH", "LINEAR HASH"} {
		t.Run(strings.ReplaceAll(method, " ", "_"), func(t *testing.T) {
			runMySQLHashPostgresPartitionIntegration(t, method)
		})
	}
}

func runMySQLHashPostgresPartitionIntegration(t *testing.T, method string) {
	srcDSN, dstDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" {
		t.Skip("需要独立 MySQL/PG 测试库及明确 DDL 标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, e := sql.Open("mysql", srcDSN)
	require.NoError(t, e)
	defer src.Close()
	dst, e := sql.Open("postgres", dstDSN)
	require.NoError(t, e)
	defer dst.Close()
	var database string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database))
	name := fmt.Sprintf("dbgold_hash_%d", time.Now().UnixNano())
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	_, e = dst.ExecContext(ctx, `CREATE SCHEMA `+q(name))
	require.NoError(t, e)
	defer dst.ExecContext(context.Background(), `DROP SCHEMA `+q(name)+` CASCADE`)
	parts := []string{name + "_a", name + "_b", name + "_c"}
	_, e = src.ExecContext(ctx, fmt.Sprintf("CREATE TABLE `%s` (`MiXeD` BIGINT NULL,`label` VARCHAR(20)) PARTITION BY %s (`MiXeD`) (PARTITION `%s`,PARTITION `%s`,PARTITION `%s`)", name, method, parts[0], parts[1], parts[2]))
	require.NoError(t, e)
	defer src.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+name+"`")
	_, e = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-9223372036854775808,'min'),(-9223372036854775807,'next'),(9223372036854775807,'max'),(NULL,'null'),(-17,'negative'),(0,'zero'),(7,'seven')")
	require.NoError(t, e)
	r, e := source.NewMySQL(srcDSN, database, source.ConnPoolConfig{})
	require.NoError(t, e)
	defer r.Close()
	w, e := target.NewPostgres(dstDSN, name, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer w.Close()
	job := &Job{LogCh: make(chan string, 8192)}
	cfg := Config{Mode: "include", Filter: name, TargetDBType: "postgres", TargetSchema: name, Content: "both", PageSize: 2, MaxParallel: 1, IntraTableParallel: 8, ValidateData: true, CaptureRepair: true}
	report := NewMigrator(r, w, job, cfg).Run(ctx)
	if report.HasFailures() {
		var raw string
		_ = dst.QueryRowContext(ctx, `SELECT pg_get_expr(c.relpartbound,c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`, name, parts[0]).Scan(&raw)
		t.Logf("native bound=%s", raw)
		if p := report.PartitionMigration.Tables[0].Plan; p != nil {
			actual, _ := w.InspectPartitionTable(ctx, name)
			ex, _ := partition.CanonicalSpec(p.Target)
			ac, _ := partition.CanonicalSpec(actual)
			a, _ := json.Marshal(ex)
			b, _ := json.Marshal(ac)
			t.Logf("expected=%s actual=%s", a, b)
		}
		t.Fatalf("HASH migration failed: partition=%+v tables=%+v data=%+v", report.PartitionMigration, report.Tables, report.Data)
	}
	result := report.PartitionMigration.Tables[0]
	require.Equal(t, method, result.Source.Method)
	require.Equal(t, "LIST", result.Plan.Target.Method)
	if method == "HASH" {
		require.Contains(t, result.Plan.Rule, "mysql-signed-modulo")
	} else {
		require.Contains(t, result.Plan.Rule, "mysql-linear-mask")
	}
	require.Len(t, report.DataValidation.Tables, 4)
	compareLeaves := func() {
		for _, p := range parts {
			read := func(db *sql.DB, query string) []string {
				rows, e := db.QueryContext(ctx, query)
				require.NoError(t, e)
				defer rows.Close()
				var labels []string
				for rows.Next() {
					var label string
					require.NoError(t, rows.Scan(&label))
					labels = append(labels, label)
				}
				require.NoError(t, rows.Err())
				return labels
			}
			a := read(src, "SELECT `label` FROM `"+name+"` PARTITION (`"+p+"`) ORDER BY `label`")
			b := read(dst, "SELECT "+q("label")+" FROM "+q(name)+"."+q(p)+" ORDER BY "+q("label"))
			require.Equal(t, a, b, "native source/target leaf %s must hold the same rows", p)
		}
	}
	compareLeaves()
	// Changing the key must route subsequent writes identically as well.
	_, e = src.ExecContext(ctx, "UPDATE `"+name+"` SET `MiXeD`=9223372036854775807 WHERE `label`='negative'")
	require.NoError(t, e)
	_, e = dst.ExecContext(ctx, "UPDATE "+q(name)+"."+q(name)+" SET "+q("MiXeD")+"=9223372036854775807 WHERE "+q("label")+"='negative'")
	require.NoError(t, e)
	compareLeaves()
	cfg.Content = "data_only"
	cfg.ValidateData = false
	cfg.CaptureRepair = false
	appended := NewMigrator(r, w, job, cfg).Run(ctx)
	require.False(t, appended.HasFailures(), "partition=%+v tables=%+v data=%+v", appended.PartitionMigration, appended.Tables, appended.Data)
	var n int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+q(name)+"."+q(name)).Scan(&n))
	require.Equal(t, 14, n)
}
