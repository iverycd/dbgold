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
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// These fixtures run only in explicitly authorized, isolated test databases.
// A skipped fixture is not evidence of product/version compatibility.
func TestMySQLPostgresPartitionIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" {
		t.Skip("需要独立 MySQL/PG 测试库及 DBGOLD_PARTITION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("postgres", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	var database, sv, dv string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT DATABASE(),VERSION()`).Scan(&database, &sv))
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT version()`).Scan(&dv))
	t.Logf("source MySQL %s, target %s", sv, dv)
	for _, method := range []string{"range", "list"} {
		t.Run(method, func(t *testing.T) {
			name := fmt.Sprintf("dbgold_p_%s_%d", method, time.Now().UnixNano())
			schema := name
			q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
			require.NoError(t, func() error { _, e := dst.ExecContext(ctx, `CREATE SCHEMA `+q(schema)); return e }())
			defer dst.ExecContext(context.Background(), `DROP SCHEMA `+q(schema)+` CASCADE`)
			parts := []string{name + "_a", name + "_b", name + "_c", name + "_empty"}
			clause := fmt.Sprintf("PARTITION BY RANGE (`k`) (PARTITION `%s` VALUES LESS THAN (0),PARTITION `%s` VALUES LESS THAN (10),PARTITION `%s` VALUES LESS THAN (20),PARTITION `%s` VALUES LESS THAN (MAXVALUE))", parts[0], parts[1], parts[2], parts[3])
			if method == "list" {
				clause = fmt.Sprintf("PARTITION BY LIST (`k`) (PARTITION `%s` VALUES IN (-1),PARTITION `%s` VALUES IN (0,9),PARTITION `%s` VALUES IN (10,11),PARTITION `%s` VALUES IN (20))", parts[0], parts[1], parts[2], parts[3])
			}
			_, err = src.ExecContext(ctx, "CREATE TABLE `"+name+"` (`k` INT NOT NULL,`payload` VARCHAR(100)) "+clause)
			require.NoError(t, err)
			defer src.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+name+"`")
			_, err = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-1,'负数'),(0,'边界'),(9,'nine'),(10,'ten'),(11,NULL)")
			require.NoError(t, err)
			r, err := source.NewMySQL(srcDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewPostgres(dstDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			job := &Job{LogCh: make(chan string, 8192)}
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "postgres", TargetSchema: schema, PageSize: 2, MaxParallel: 2, IntraTableParallel: 4, ValidateData: true, CaptureRepair: true}
			m := NewMigrator(r, w, job, cfg)
			preview := m.PreflightPartitions(ctx)
			require.True(t, preview.Ready, "%+v", preview)
			report := m.Run(ctx)
			if report.HasFailures() {
				if entry := report.PartitionMigration.Tables[0]; entry.Plan != nil {
					actual, _ := w.InspectPartitionTable(ctx, name)
					expected, _ := partition.CanonicalSpec(entry.Plan.Target)
					got, _ := partition.CanonicalSpec(actual)
					a, _ := json.Marshal(expected)
					b, _ := json.Marshal(got)
					t.Logf("expected=%s actual=%s", a, b)
				}
				t.Fatalf("partition migration failed: tables=%+v data=%+v validation=%+v", report.Tables, report.Data, report.DataValidation)
			}
			require.NotNil(t, report.PartitionMigration)
			require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
			require.NotNil(t, report.RepairManifest)
			var saved repair.Object
			for _, o := range report.RepairManifest.Objects {
				if o.Kind == "table" && o.SourceTable == name {
					saved = o
				}
			}
			require.NotNil(t, saved.PartitionPlan)
			require.Len(t, report.DataValidation.Tables, 5, "one root plus four independent leaf content checks")
			for _, p := range parts {
				var n int
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT count(*) FROM `+q(schema)+`.`+q(p)).Scan(&n))
				if p == parts[3] {
					require.Zero(t, n)
				}
			}
			// Only-data preserves append semantics, with per-leaf baseline deltas.
			cfg.Content = "data_only"
			cfg.ValidateData = false
			cfg.CaptureRepair = false
			appendReport := NewMigrator(r, w, job, cfg).Run(ctx)
			if appendReport.HasFailures() {
				t.Fatalf("partition append failed: tables=%+v data=%+v counts=%+v", appendReport.Tables, appendReport.Data, appendReport.RowCounts)
			}
			var n int
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT count(*) FROM `+q(schema)+`.`+q(name)).Scan(&n))
			require.Equal(t, 10, n)
			// A missing empty leaf must fail structural inspection, despite root existence.
			_, err = dst.ExecContext(ctx, `DROP TABLE `+q(schema)+`.`+q(parts[3]))
			require.NoError(t, err)
			verification, err := target.InspectRepair(ctx, w, saved)
			require.NoError(t, err)
			require.NotEqual(t, "succeeded", verification.Status)
			bad := NewMigrator(r, w, job, cfg).Run(ctx)
			require.True(t, bad.HasFailures())
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT count(*) FROM `+q(schema)+`.`+q(name)).Scan(&n))
			require.Equal(t, 10, n, "incompatible target must remain unwritten")
		})
	}
}
