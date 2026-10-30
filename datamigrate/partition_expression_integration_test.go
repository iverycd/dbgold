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

func TestMySQLComplexExpressionPostgresPartitionIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_PG_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" {
		t.Skip("需要独立 MySQL/PG 实库及明确 DDL 标志")
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
	require.NoError(t, src.QueryRowContext(ctx, "SELECT DATABASE(),VERSION()").Scan(&database, &sv))
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT version()").Scan(&dv))
	t.Logf("source MySQL %s, target %s", sv, dv)
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, method := range []string{"RANGE", "LIST", "HASH", "LINEAR HASH"} {
		t.Run(strings.ReplaceAll(method, " ", "_"), func(t *testing.T) {
			name := fmt.Sprintf("dbgold_expr_%d", time.Now().UnixNano())
			_, err := dst.ExecContext(ctx, "CREATE SCHEMA "+q(name))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, e := dst.ExecContext(context.Background(), "DROP SCHEMA "+q(name)+" CASCADE")
				require.NoError(t, e)
			})
			parts := []string{name + "_a", name + "_b", name + "_c"}
			expr := "MOD(ABS((`MiXeD` * 3 + `b`) DIV 5),7)"
			nullable, values := "NULL", ",(NULL,0,'null')"
			defs := fmt.Sprintf("PARTITION `%s`,PARTITION `%s`,PARTITION `%s`", parts[0], parts[1], parts[2])
			switch method {
			case "RANGE":
				nullable, values = "NOT NULL", ""
				defs = fmt.Sprintf("PARTITION `%s` VALUES LESS THAN (1),PARTITION `%s` VALUES LESS THAN (4),PARTITION `%s` VALUES LESS THAN (MAXVALUE)", parts[0], parts[1], parts[2])
			case "LIST":
				defs = fmt.Sprintf("PARTITION `%s` VALUES IN (0,3),PARTITION `%s` VALUES IN (1,4),PARTITION `%s` VALUES IN (2,5,6,NULL)", parts[0], parts[1], parts[2])
			default:
				expr = "(`MiXeD` * 3 + `b`) DIV 5"
			}
			_, err = src.ExecContext(ctx, fmt.Sprintf("CREATE TABLE `%s` (`MiXeD` INT %s,`b` SMALLINT NOT NULL,`label` VARCHAR(40)) PARTITION BY %s (%s) (%s)", name, nullable, method, expr, defs))
			require.NoError(t, err)
			t.Cleanup(func() { _, e := src.ExecContext(context.Background(), "DROP TABLE `"+name+"`"); require.NoError(t, e) })
			_, err = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-2147483648,-32768,'min'),(2147483647,32767,'max'),(-17,-7,'negative'),(-1,0,'trunc'),(0,0,'zero'),(7,5,'positive'),(7,5,'positive')"+values)
			require.NoError(t, err)
			r, err := source.NewMySQL(srcDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewPostgres(dstDSN, name, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "postgres", TargetSchema: name, PageSize: 2, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
			job := &Job{LogCh: make(chan string, 8192)}
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			if report.HasFailures() {
				if report.PartitionMigration != nil && len(report.PartitionMigration.Tables) > 0 {
					entry := report.PartitionMigration.Tables[0]
					if entry.Plan != nil {
						actual, _ := w.InspectPartitionTable(ctx, name)
						ex, _ := partition.CanonicalSpec(entry.Plan.Target)
						ac, _ := partition.CanonicalSpec(actual)
						a, _ := json.Marshal(ex)
						b, _ := json.Marshal(ac)
						t.Logf("expected=%s actual=%s", a, b)
					}
					t.Logf("partition=%+v", entry)
				}
				t.Fatalf("expression migration failed: tables=%+v data=%+v validation=%+v", report.Tables, report.Data, report.DataValidation)
			}
			require.Len(t, report.DataValidation.Tables, 4)
			compare := func() {
				for _, leaf := range parts {
					read := func(db *sql.DB, query string) []string {
						rows, e := db.QueryContext(ctx, query)
						require.NoError(t, e)
						defer rows.Close()
						var result []string
						for rows.Next() {
							var v string
							require.NoError(t, rows.Scan(&v))
							result = append(result, v)
						}
						require.NoError(t, rows.Err())
						return result
					}
					a := read(src, "SELECT label FROM `"+name+"` PARTITION (`"+leaf+"`) ORDER BY label")
					b := read(dst, "SELECT label FROM "+q(name)+"."+q(leaf)+" ORDER BY label")
					require.Equal(t, a, b, "leaf %s", leaf)
				}
			}
			compare()
			for _, statement := range []string{"INSERT INTO %s VALUES (-55,17,'future')", "UPDATE %s SET %s=123,b=-8 WHERE label='negative'"} {
				if strings.HasPrefix(statement, "INSERT") {
					_, err = src.ExecContext(ctx, fmt.Sprintf(statement, "`"+name+"`"))
					require.NoError(t, err)
					_, err = dst.ExecContext(ctx, fmt.Sprintf(statement, q(name)+"."+q(name)))
				} else {
					_, err = src.ExecContext(ctx, fmt.Sprintf(statement, "`"+name+"`", "`MiXeD`"))
					require.NoError(t, err)
					_, err = dst.ExecContext(ctx, fmt.Sprintf(statement, q(name)+"."+q(name), q("MiXeD")))
				}
				require.NoError(t, err)
				compare()
			}
			cfg.Content, cfg.CaptureRepair = "data_only", false
			appended := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, appended.HasFailures(), "partition=%+v data=%+v validation=%+v", appended.PartitionMigration, appended.Data, appended.DataValidation)
			require.Len(t, appended.DataValidation.Tables, 4)
		})
	}
}
