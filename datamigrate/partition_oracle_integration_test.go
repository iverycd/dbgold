package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Run only against dedicated test databases. This test creates no users,
// instances, filegroups or tablespaces, and cleans up only its unique tables.
// An unset environment skips the fixture; a skip never certifies Oracle.
func TestMySQLOraclePartitionIntegration(t *testing.T) {
	srcDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN")
	dstDSN := os.Getenv("DBGOLD_PARTITION_ORACLE_DSN")
	schema := os.Getenv("DBGOLD_PARTITION_ORACLE_SCHEMA")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || schema == "" {
		t.Skip("requires isolated MySQL/Oracle fixtures and DBGOLD_PARTITION_ALLOW_DDL=1")
	}
	require.True(t, strings.HasPrefix(schema, "DBGOLD_"), "Oracle test schema must start with DBGOLD_")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("oracle", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	var database, mysqlVersion, oracleVersion string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT DATABASE(),VERSION()`).Scan(&database, &mysqlVersion))
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT BANNER FROM V$VERSION WHERE BANNER LIKE 'Oracle%' AND ROWNUM=1`).Scan(&oracleVersion))
	t.Logf("source MySQL %s; target %s", mysqlVersion, oracleVersion)
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, method := range []string{"RANGE", "LIST"} {
		t.Run(method, func(t *testing.T) {
			name := "Po" + method[:1] + strconv.FormatInt(time.Now().UnixNano(), 36)
			parts := []string{name + "_Neg", name + "_Mid", name + "_High", name + "_Empty"}
			root := quote(schema) + "." + quote(strings.ToUpper(name))
			clause := fmt.Sprintf("PARTITION BY RANGE (`K`) (PARTITION `%s` VALUES LESS THAN(0),PARTITION `%s` VALUES LESS THAN(10),PARTITION `%s` VALUES LESS THAN(20),PARTITION `%s` VALUES LESS THAN(MAXVALUE))", parts[0], parts[1], parts[2], parts[3])
			if method == "LIST" {
				clause = fmt.Sprintf("PARTITION BY LIST (`K`) (PARTITION `%s` VALUES IN(-1),PARTITION `%s` VALUES IN(0,9),PARTITION `%s` VALUES IN(10,11),PARTITION `%s` VALUES IN(20))", parts[0], parts[1], parts[2], parts[3])
			}
			_, err := src.ExecContext(ctx, "CREATE TABLE `"+name+"` (`K` INT NOT NULL,`Payload` VARCHAR(40)) "+clause)
			require.NoError(t, err)
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				_, _ = src.ExecContext(cleanup, "DROP TABLE IF EXISTS `"+name+"`")
				_, _ = dst.ExecContext(cleanup, "DROP TABLE "+root+" CASCADE CONSTRAINTS PURGE")
			}()
			_, err = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-1,'negative'),(0,'boundary'),(9,'nine'),(10,'ten'),(11,NULL)")
			require.NoError(t, err)
			r, err := source.NewMySQLForOracle(srcDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewOracle(dstDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			job := &Job{LogCh: make(chan string, 8192)}
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "oracle", TargetSchema: schema, PageSize: 2, MaxParallel: 1, IntraTableParallel: 4, ValidateData: true, CaptureRepair: true}
			m := NewMigrator(r, w, job, cfg)
			preview := m.PreflightPartitions(ctx)
			require.True(t, preview.Ready, "partition preflight: %+v", preview)
			report := m.Run(ctx)
			if report.HasFailures() {
				var details []string
				if report.PartitionMigration != nil {
					for _, entry := range report.PartitionMigration.Tables {
						details = append(details, entry.SourceTable+": "+entry.Status+" "+entry.Error)
					}
				}
				t.Fatalf("partition migration failed: %s; tables=%+v data=%+v validation=%+v", strings.Join(details, "; "), report.Tables, report.Data, report.DataValidation)
			}
			require.NotNil(t, report.PartitionMigration)
			entry := report.PartitionMigration.Tables[0]
			require.Equal(t, "succeeded", entry.Status)
			require.Equal(t, method, entry.Plan.Target.Method)
			require.Equal(t, "enabled", entry.Plan.Target.RowMovement)
			require.NotNil(t, report.DataValidation)
			require.Len(t, report.DataValidation.Tables, 5, "content check must cover the logical root and all four physical leaves")
			var movement string
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT ROW_MOVEMENT FROM ALL_TABLES WHERE OWNER=:1 AND TABLE_NAME=:2`, schema, strings.ToUpper(name)).Scan(&movement))
			require.Equal(t, "ENABLED", movement, "MySQL accepts updates that move rows between partitions")
			actual, err := w.InspectPartitionTable(ctx, strings.ToUpper(name))
			require.NoError(t, err)
			require.NoError(t, partition.Equivalent(entry.Plan.Target, actual))
			read := func(db *sql.DB, query string) []string {
				rows, err := db.QueryContext(ctx, query)
				require.NoError(t, err)
				defer rows.Close()
				var out []string
				for rows.Next() {
					var key int64
					var label sql.NullString
					require.NoError(t, rows.Scan(&key, &label))
					out = append(out, fmt.Sprintf("%d|%t|%s", key, label.Valid, label.String))
				}
				require.NoError(t, rows.Err())
				return out
			}
			compareLeaves := func() {
				for _, leaf := range parts {
					sourceRows := read(src, "SELECT `K`,`Payload` FROM `"+name+"` PARTITION (`"+leaf+"`) ORDER BY `K`,`Payload`")
					targetRows := read(dst, "SELECT "+quote("K")+","+quote("PAYLOAD")+" FROM "+root+" PARTITION ("+quote(strings.ToUpper(leaf))+") ORDER BY "+quote("K")+","+quote("PAYLOAD"))
					require.Equal(t, sourceRows, targetRows, "native leaf %s", leaf)
				}
			}
			compareLeaves()
			_, err = src.ExecContext(ctx, "UPDATE `"+name+"` SET `K`=10 WHERE `Payload`='negative'")
			require.NoError(t, err)
			_, err = dst.ExecContext(ctx, "UPDATE "+root+" SET "+quote("K")+"=10 WHERE "+quote("PAYLOAD")+"='negative'")
			require.NoError(t, err, "target must preserve cross-partition update behavior")
			compareLeaves()
			cfg.Content, cfg.ValidateData, cfg.CaptureRepair = "data_only", false, false
			appendReport := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, appendReport.HasFailures(), "append failed: tables=%+v data=%+v", appendReport.Tables, appendReport.Data)
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+root).Scan(&count))
			require.Equal(t, 10, count)
			_, err = dst.ExecContext(ctx, "ALTER TABLE "+root+" DISABLE ROW MOVEMENT")
			require.NoError(t, err)
			blocked := NewMigrator(r, w, job, cfg).Run(ctx)
			require.True(t, blocked.HasFailures(), "changed row-movement behavior must fail before append")
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+root).Scan(&count))
			require.Equal(t, 10, count, "incompatible existing target must remain unwritten")
		})
	}
}
