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
	"github.com/stretchr/testify/require"
)

// Only explicit disposable fixtures may opt into this test. It creates its own
// namespace and tables, and does not alter database-wide settings.
func TestMySQLDamengPartitionIntegration(t *testing.T) {
	myDSN, dmDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_DM_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || myDSN == "" || dmDSN == "" {
		t.Skip("需要隔离 MySQL/DM8 测试连接及明确的 DDL 开关")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	dm, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	defer dm.Close()
	var database, sourceVersion, targetVersion string
	require.NoError(t, my.QueryRowContext(ctx, `SELECT DATABASE(),VERSION()`).Scan(&database, &sourceVersion))
	require.NoError(t, dm.QueryRowContext(ctx, `SELECT SVR_VERSION FROM V$INSTANCE`).Scan(&targetVersion))
	t.Logf("MySQL %s → %s", sourceVersion, strings.TrimSpace(targetVersion))
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	for _, method := range []string{"RANGE", "LIST"} {
		t.Run(method, func(t *testing.T) {
			name := fmt.Sprintf("DMP_%s_%d", method, time.Now().UnixNano())
			schema := name
			_, err := dm.ExecContext(ctx, "CREATE SCHEMA "+quote(schema)+" AUTHORIZATION SYSDBA")
			require.NoError(t, err)
			defer dm.ExecContext(context.Background(), "DROP SCHEMA "+quote(schema)+" CASCADE")
			parts := []string{"LOW", "MIDDLE", "HIGH", "EMPTY"}
			clause := "PARTITION BY RANGE (`K`) (PARTITION `LOW` VALUES LESS THAN (0),PARTITION `MIDDLE` VALUES LESS THAN (10),PARTITION `HIGH` VALUES LESS THAN (20),PARTITION `EMPTY` VALUES LESS THAN (MAXVALUE))"
			if method == "LIST" {
				clause = "PARTITION BY LIST (`K`) (PARTITION `LOW` VALUES IN (-1),PARTITION `MIDDLE` VALUES IN (0,9),PARTITION `HIGH` VALUES IN (10,11),PARTITION `EMPTY` VALUES IN (20))"
			}
			_, err = my.ExecContext(ctx, "CREATE TABLE `"+name+"` (`K` INT NOT NULL,`PAYLOAD` VARCHAR(80)) "+clause)
			require.NoError(t, err)
			defer my.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+name+"`")
			_, err = my.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-1,'负数'),(0,'边界'),(9,'中文'),(10,'高区'),(11,NULL)")
			require.NoError(t, err)
			r, err := source.NewMySQLForDameng(myDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewDaMeng(dmDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetSchema: schema, TargetDBType: "dameng", PageSize: 2, MaxParallel: 2, IntraTableParallel: 4, ValidateData: true, CaptureRepair: true}
			job := &Job{LogCh: make(chan string, 16384)}
			preflight := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
			if !preflight.Ready {
				t.Fatalf("preflight issues=%+v", preflight.Issues)
			}
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			if report.HasFailures() {
				t.Fatalf("tables=%+v data=%+v partitions=%+v validation=%+v", report.Tables, report.Data, report.PartitionMigration, report.DataValidation)
			}
			require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
			require.NotNil(t, report.RepairManifest)
			counts, err := w.CountPartitionRows(ctx, name, report.PartitionMigration.Tables[0].Plan.Target)
			require.NoError(t, err)
			require.Equal(t, int64(0), counts[schema+".EMPTY"])
			require.Len(t, report.DataValidation.Tables, 5)
			for _, leaf := range parts {
				var count int64
				require.NoError(t, dm.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quote(schema)+"."+quote(name)+" PARTITION ("+quote(leaf)+")").Scan(&count))
			}
			_, err = dm.ExecContext(ctx, "INSERT INTO "+quote(schema)+"."+quote(name)+" (K,PAYLOAD) VALUES(20,'future')")
			require.NoError(t, err)
			var rowCount int64
			require.NoError(t, dm.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quote(schema)+"."+quote(name)+" PARTITION (\"EMPTY\")").Scan(&rowCount))
			require.Equal(t, int64(1), rowCount)
		})
	}
}

func TestOracleDamengPartitionIntegration(t *testing.T) {
	oraDSN, owner, dmDSN := os.Getenv("DBGOLD_PARTITION_ORACLE_DSN"), os.Getenv("DBGOLD_PARTITION_ORACLE_SCHEMA"), os.Getenv("DBGOLD_PARTITION_DM_DSN")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || oraDSN == "" || owner == "" || dmDSN == "" {
		t.Skip("需要独立 Oracle/DM8 测试连接及明确的 DDL 开关")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ora, err := sql.Open("oracle", oraDSN)
	require.NoError(t, err)
	defer ora.Close()
	dm, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	defer dm.Close()
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	for _, method := range []string{"RANGE", "LIST"} {
		t.Run(method, func(t *testing.T) {
			name := fmt.Sprintf("ODP_%s_%X", method, time.Now().UnixNano())
			schema := name
			_, err := dm.ExecContext(ctx, "CREATE SCHEMA "+quote(schema)+" AUTHORIZATION SYSDBA")
			require.NoError(t, err)
			defer dm.ExecContext(context.Background(), "DROP SCHEMA "+quote(schema)+" CASCADE")
			clause := `PARTITION BY RANGE("K") (PARTITION "LOW" VALUES LESS THAN(0),PARTITION "MIDDLE" VALUES LESS THAN(10),PARTITION "HIGH" VALUES LESS THAN(20),PARTITION "EMPTY" VALUES LESS THAN(MAXVALUE))`
			if method == "LIST" {
				clause = `PARTITION BY LIST("K") (PARTITION "LOW" VALUES(-1),PARTITION "MIDDLE" VALUES(0,9),PARTITION "HIGH" VALUES(10,11),PARTITION "EMPTY" VALUES(20))`
			}
			qn := quote(owner) + "." + quote(name)
			_, err = ora.ExecContext(ctx, "CREATE TABLE "+qn+` ("K" NUMBER(10) NOT NULL,"PAYLOAD" VARCHAR2(80)) `+clause)
			require.NoError(t, err)
			defer ora.ExecContext(context.Background(), "DROP TABLE "+qn+" CASCADE CONSTRAINTS PURGE")
			for _, value := range []int{-1, 0, 9, 10, 11} {
				_, err = ora.ExecContext(ctx, "INSERT INTO "+qn+`("K","PAYLOAD") VALUES(:1,:2)`, value, fmt.Sprint("原值", value))
				require.NoError(t, err)
			}
			r, err := source.NewOracleForDameng(oraDSN, owner, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewDaMeng(dmDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetSchema: schema, TargetDBType: "dameng", PageSize: 2, MaxParallel: 1, ValidateData: true, CaptureRepair: true}
			job := &Job{LogCh: make(chan string, 16384)}
			p := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
			if !p.Ready {
				t.Fatalf("preflight=%+v", p.Issues)
			}
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			if report.HasFailures() {
				t.Fatalf("tables=%+v data=%+v partitions=%+v validation=%+v", report.Tables, report.Data, report.PartitionMigration, report.DataValidation)
			}
			require.Equal(t, "succeeded", report.PartitionMigration.Tables[0].Status)
			require.Equal(t, "disabled", report.PartitionMigration.Tables[0].Plan.Target.RowMovement)
			require.Len(t, report.DataValidation.Tables, 5)
			var count int64
			require.NoError(t, dm.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quote(schema)+"."+quote(name)+` PARTITION("EMPTY")`).Scan(&count))
			require.Zero(t, count)
			_, err = dm.ExecContext(ctx, "INSERT INTO "+quote(schema)+"."+quote(name)+`("K","PAYLOAD") VALUES(20,'future')`)
			require.NoError(t, err)
		})
	}
}
