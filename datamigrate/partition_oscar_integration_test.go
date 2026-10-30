package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

// Only disposable test databases may opt in. The test uses unique table names
// in the configured schemas, never drops a schema, and never probes by mutating
// existing tables. A skip is not evidence of Oscar product/version acceptance.
func TestMySQLOscarPartitionIntegration(t *testing.T) {
	myDSN := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN")
	url, user, password, schema := os.Getenv("OSCAR_TEST_URL"), os.Getenv("OSCAR_TEST_USER"), os.Getenv("OSCAR_TEST_PASSWORD"), os.Getenv("OSCAR_TEST_SCHEMA")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || os.Getenv("DBGOLD_PARTITION_OSCAR_ENABLE") != "1" || myDSN == "" || url == "" || user == "" || password == "" || schema == "" {
		t.Skip("需要隔离 MySQL、OSCAR_TEST_URL/USER/PASSWORD/SCHEMA，以及 DBGOLD_PARTITION_ALLOW_DDL=1 和 DBGOLD_PARTITION_OSCAR_ENABLE=1")
	}
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	bridgeJar, jdbcJar := os.Getenv("OSCAR_BRIDGE_JAR"), os.Getenv("OSCAR_JDBC_JAR")
	if bridgeJar == "" {
		bridgeJar = filepath.Join(root, "jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar")
	}
	if jdbcJar == "" {
		jdbcJar = filepath.Join(root, "third_party", "oscar", "oscarJDBC8.jar")
	}
	// NewOscar uses the normal application bridge. This test is intentionally
	// not parallel, so all sessions share the same explicitly configured JVM.
	jdbcbridge.ConfigureDefault(jdbcbridge.Config{JavaBin: os.Getenv("OSCAR_JAVA_BIN"), BridgeJar: bridgeJar, JDBCJar: jdbcJar})
	defer jdbcbridge.ConfigureDefault(jdbcbridge.Config{})
	manager := jdbcbridge.Default()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	session, err := manager.OpenSession(ctx, jdbcbridge.SessionOptions{URL: url, Username: user, Password: password, Schema: schema, MaxOpenConns: 3, MaxIdleConns: 2})
	require.NoError(t, err)
	defer manager.CloseSession(context.Background(), session)
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	var database, mysqlVersion string
	require.NoError(t, my.QueryRowContext(ctx, `SELECT DATABASE(),VERSION()`).Scan(&database, &mysqlVersion))
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, method := range []string{"RANGE", "LIST"} {
		t.Run(method, func(t *testing.T) {
			name := fmt.Sprintf("DBGOLD_OSCAR_P_%s_%X", method, time.Now().UnixNano())
			qualified := quote(schema) + "." + quote(name)
			clause := "PARTITION BY RANGE (`K`) (PARTITION `LOW` VALUES LESS THAN(0),PARTITION `MIDDLE` VALUES LESS THAN(10),PARTITION `HIGH` VALUES LESS THAN(20),PARTITION `EMPTY` VALUES LESS THAN(MAXVALUE))"
			if method == "LIST" {
				clause = "PARTITION BY LIST (`K`) (PARTITION `LOW` VALUES IN(-1),PARTITION `MIDDLE` VALUES IN(0,9),PARTITION `HIGH` VALUES IN(10,11),PARTITION `EMPTY` VALUES IN(20))"
			}
			_, err := my.ExecContext(ctx, "CREATE TABLE `"+name+"` (`K` INT NOT NULL,`PAYLOAD` VARCHAR(80)) "+clause)
			require.NoError(t, err)
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cleanupCancel()
				if _, cleanupErr := my.ExecContext(cleanupCtx, "DROP TABLE IF EXISTS `"+name+"`"); cleanupErr != nil {
					t.Errorf("cleanup isolated MySQL fixture %s: %v", name, cleanupErr)
				}
				if cleanupErr := manager.Exec(cleanupCtx, session, "DROP TABLE IF EXISTS "+qualified+" CASCADE"); cleanupErr != nil {
					t.Errorf("cleanup isolated Oscar fixture %s: %v", name, cleanupErr)
				}
			}()
			_, err = my.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES(-1,'negative'),(0,'边界'),(9,'尾空格 '),(10,'high'),(11,NULL)")
			require.NoError(t, err)
			r, err := source.NewMySQL(myDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewOscar(url, user, password, schema, target.ConnPoolConfig{MaxOpenConns: 4, MaxIdleConns: 2})
			require.NoError(t, err)
			defer w.Close()
			profile, err := w.InspectPartitionProfile(ctx)
			require.NoError(t, err)
			t.Logf("MySQL %s → %s; native mode=%s", mysqlVersion, profile.Version, profile.Mode)
			job := &Job{LogCh: make(chan string, 16384)}
			cfg := Config{Mode: "include", Filter: name, Content: "both", TargetDBType: "oscar", TargetSchema: schema, PageSize: 2, MaxParallel: 1, IntraTableParallel: 3, ValidateData: true, CaptureRepair: true}
			preflight := NewMigrator(r, w, job, cfg).PreflightPartitions(ctx)
			require.True(t, preflight.Ready, "partition issues=%+v", preflight.Issues)
			report := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, report.HasFailures(), "tables=%+v data=%+v partitions=%+v validation=%+v", report.Tables, report.Data, report.PartitionMigration, report.DataValidation)
			require.NotNil(t, report.PartitionMigration)
			require.Len(t, report.PartitionMigration.Tables, 1)
			entry := report.PartitionMigration.Tables[0]
			require.Equal(t, "succeeded", entry.Status)
			require.NotNil(t, report.RepairManifest)
			require.NotNil(t, report.DataValidation)
			require.Len(t, report.DataValidation.Tables, 5, "root and all four physical leaves must be checked")
			for _, result := range report.DataValidation.Tables {
				require.Equal(t, validation.StatusPassed, result.Status, "%+v", result)
			}
			counts, err := w.CountPartitionRows(ctx, name, entry.Plan.Target)
			require.NoError(t, err)
			require.Zero(t, counts[partition.LeafKey(schema, "EMPTY")])
			compareLeaves := func(multiplier int64) {
				want, err := r.CountPartitionRows(ctx, name, entry.Source)
				require.NoError(t, err)
				got, err := w.CountPartitionRows(ctx, name, entry.Plan.Target)
				require.NoError(t, err)
				for _, m := range entry.Plan.Mapping {
					require.Equal(t, multiplier*want[partition.LeafKey(m.SourceSchema, m.SourceName)], got[partition.LeafKey(m.TargetSchema, m.TargetName)], "leaf %s", m.SourceName)
				}
			}
			compareLeaves(1)
			// Verify future INSERT routing and a key-changing UPDATE on both
			// engines, including movement into an originally empty leaf.
			_, err = my.ExecContext(ctx, "UPDATE `"+name+"` SET `K`=20 WHERE `PAYLOAD`='negative'")
			require.NoError(t, err)
			require.NoError(t, manager.Exec(ctx, session, "UPDATE "+qualified+` SET "K"=20 WHERE "PAYLOAD"='negative'`))
			_, err = my.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES(9,'future')")
			require.NoError(t, err)
			require.NoError(t, manager.Exec(ctx, session, "INSERT INTO "+qualified+`("K","PAYLOAD") VALUES(9,'future')`))
			compareLeaves(1)
			counts, err = w.CountPartitionRows(ctx, name, entry.Plan.Target)
			require.NoError(t, err)
			require.Equal(t, int64(1), counts[partition.LeafKey(schema, "EMPTY")])
			cfg.Content, cfg.ValidateData, cfg.CaptureRepair = "data_only", true, false
			appended := NewMigrator(r, w, job, cfg).Run(ctx)
			require.False(t, appended.HasFailures(), "data=%+v partitions=%+v validation=%+v", appended.Data, appended.PartitionMigration, appended.DataValidation)
			require.NotNil(t, appended.DataValidation)
			require.Len(t, appended.DataValidation.Tables, 5, "append must validate the frozen root and all four leaf multisets")
			for _, result := range appended.DataValidation.Tables {
				require.Equal(t, validation.StatusPassed, result.Status, "%+v", result)
			}
			require.Equal(t, int64(6), appended.DataValidation.Tables[0].TargetRows, "root validation reports the copied increment")
			compareLeaves(2)
			n, err := manager.QueryInt64(ctx, session, "SELECT COUNT(*) FROM "+qualified)
			require.NoError(t, err)
			require.Equal(t, int64(12), n)
		})
	}
}
