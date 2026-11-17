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

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// TestOscarMySQLIntegration uses isolated, pre-created schemas and unique
// fixture object names. Enable with DBGOLD_OSCAR_MYSQL_ALLOW_DDL=1, the four
// OSCAR_TEST_* variables, and either or both version-specific MySQL DSNs.
func TestOscarMySQLIntegration(t *testing.T) {
	url, user, password, schema := os.Getenv("OSCAR_TEST_URL"), os.Getenv("OSCAR_TEST_USER"), os.Getenv("OSCAR_TEST_PASSWORD"), os.Getenv("OSCAR_TEST_SCHEMA")
	if os.Getenv("DBGOLD_OSCAR_MYSQL_ALLOW_DDL") != "1" || url == "" || user == "" || password == "" || schema == "" {
		t.Skip("requires isolated OSCAR_TEST_URL/USER/PASSWORD/SCHEMA and DBGOLD_OSCAR_MYSQL_ALLOW_DDL=1")
	}
	versions := []struct {
		name, dsnEnv string
		major        int
		minor        int
	}{
		{"5.7", "DBGOLD_OSCAR_MYSQL57_DSN", 5, 7},
		{"8.0", "DBGOLD_OSCAR_MYSQL80_DSN", 8, 0},
	}
	if os.Getenv(versions[0].dsnEnv) == "" && os.Getenv(versions[1].dsnEnv) == "" {
		t.Skip("set DBGOLD_OSCAR_MYSQL57_DSN and/or DBGOLD_OSCAR_MYSQL80_DSN to isolated test databases")
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
	jdbcbridge.ConfigureDefault(jdbcbridge.Config{JavaBin: os.Getenv("OSCAR_JAVA_BIN"), BridgeJar: bridgeJar, JDBCJar: jdbcJar})
	t.Cleanup(func() { jdbcbridge.ConfigureDefault(jdbcbridge.Config{}) })
	manager := jdbcbridge.Default()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	session, err := manager.OpenSession(ctx, jdbcbridge.SessionOptions{URL: url, Username: user, Password: password, MaxOpenConns: 4, MaxIdleConns: 2})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.CloseSession(cleanup, session); err != nil {
			t.Errorf("close isolated Oscar fixture session: %v", err)
		}
	})
	fixtureID := fmt.Sprintf("DbgOM_%x", time.Now().UnixNano())
	ordinary, rangeTable, listTable, view := fixtureID+"_Ord", fixtureID+"_Range", fixtureID+"_List", fixtureID+"_View"
	oscarQuote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	oscarTable := func(name string) string { return oscarQuote(schema) + "." + oscarQuote(name) }
	createOscar := func(kind, name, definition string) {
		t.Helper()
		require.NoError(t, manager.Exec(ctx, session, "CREATE "+kind+" "+oscarTable(name)+" "+definition))
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := manager.Exec(cleanup, session, "DROP "+kind+" IF EXISTS "+oscarTable(name)); err != nil {
				t.Errorf("drop isolated Oscar %s %s: %v", kind, name, err)
			}
		})
	}
	createOscar("TABLE", ordinary, `("Id" INTEGER PRIMARY KEY, "Payload" VARCHAR(40), "Amount" DECIMAL(12,3))`)
	require.NoError(t, manager.Exec(ctx, session, "INSERT INTO "+oscarTable(ordinary)+` VALUES (7,'边界 ',123456789.012)`))
	createOscar("TABLE", rangeTable, `("K" INTEGER NOT NULL, "Payload" VARCHAR(40)) PARTITION BY RANGE ("K") (
PARTITION "Low" VALUES LESS THAN (0), PARTITION "Mid" VALUES LESS THAN (10), PARTITION "High" VALUES LESS THAN (20), PARTITION "Empty" VALUES LESS THAN (MAXVALUE))`)
	createOscar("TABLE", listTable, `("K" INTEGER NOT NULL, "Payload" VARCHAR(40)) PARTITION BY LIST ("K") (
PARTITION "Low" VALUES (-1), PARTITION "Mid" VALUES (0,9), PARTITION "High" VALUES (10,11), PARTITION "Empty" VALUES (20))`)
	for _, table := range []string{rangeTable, listTable} {
		for _, values := range []string{"(-1,'negative')", "(0,'边界 ')", "(9,'edge')", "(10,NULL)", "(11,'duplicate')", "(11,'duplicate')"} {
			require.NoError(t, manager.Exec(ctx, session, "INSERT INTO "+oscarTable(table)+" VALUES "+values))
		}
	}
	createOscar("VIEW", view, `AS SELECT "Id", "Payload" FROM `+oscarTable(ordinary)+` WHERE "Id" > 0`)
	for _, expected := range versions {
		t.Run(expected.name, func(t *testing.T) {
			dsn := os.Getenv(expected.dsnEnv)
			if dsn == "" {
				t.Skipf("set %s to test MySQL %s", expected.dsnEnv, expected.name)
			}
			setup, err := mysql.ParseDSN(dsn)
			require.NoError(t, err, "invalid isolated MySQL test DSN")
			require.True(t, strings.HasPrefix(strings.ToLower(setup.DBName), "dbgold_"), "MySQL fixture database must start with dbgold_")
			setup.ParseTime = true
			setup.MultiStatements = false
			if setup.Params == nil {
				setup.Params = map[string]string{}
			}
			setup.Params["charset"] = "utf8mb4"
			dsn = setup.FormatDSN()
			db, err := sql.Open("mysql", dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			var dstSchema, rawVersion string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT DATABASE(), VERSION()`).Scan(&dstSchema, &rawVersion))
			require.Equal(t, setup.DBName, dstSchema)
			version, err := dialect.ParseMySQLVersion(rawVersion)
			require.NoError(t, err)
			require.Equal(t, expected.major, version.Major)
			require.Equal(t, expected.minor, version.Minor)
			t.Logf("Oscar → MySQL %s", rawVersion)
			writer, err := target.NewMySQL(dsn, dstSchema, target.ConnPoolConfig{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = writer.Close() })
			mysqlTable := func(name string) string { return writer.Dialect().QualifyTable(dstSchema, strings.ToLower(name)) }
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if _, err := db.ExecContext(cleanup, "DROP VIEW IF EXISTS "+mysqlTable(view)); err != nil {
					t.Errorf("drop isolated MySQL fixture view: %v", err)
				}
				for _, name := range []string{ordinary, rangeTable, listTable} {
					if _, err := db.ExecContext(cleanup, "DROP TABLE IF EXISTS "+mysqlTable(name)); err != nil {
						t.Errorf("drop isolated MySQL fixture table %s: %v", name, err)
					}
				}
			})
			reader, err := source.NewOscarContext(ctx, url, user, password, schema, source.ConnPoolConfig{MaxOpenConns: 4, MaxIdleConns: 2})
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.Close() })
			cfg := Config{
				Mode: "include", Filter: strings.Join([]string{ordinary, rangeTable, listTable}, ","), Content: "both",
				TargetDBType: "mysql", TargetSchema: dstSchema, PageSize: 2, MaxParallel: 2,
				LowerCaseNames: true, ValidateData: true, CaptureRepair: true,
			}
			job := &Job{LogCh: make(chan string, 16384)}
			preview := NewMigrator(reader, writer, job, cfg).PreflightPartitions(ctx)
			require.True(t, preview.Ready, "partition issues=%+v; fatal=%s", preview.Issues, preview.FatalError)
			report := NewMigrator(reader, writer, job, cfg).Run(ctx)
			require.False(t, report.HasFailures(), "report=%+v", report)
			require.Equal(t, 3, report.Tables.Success)
			require.Equal(t, 3, report.Data.Success)
			require.NotNil(t, report.RepairManifest)
			require.NotNil(t, report.DataValidation)
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			require.Len(t, report.DataValidation.Tables, 11, "ordinary table, two logical roots, and all eight leaf partitions")
			require.NotNil(t, report.PartitionMigration)
			for _, entry := range report.PartitionMigration.Tables {
				if entry.SourceTable == ordinary {
					require.Equal(t, "ordinary", entry.Status)
					continue
				}
				require.Equal(t, "succeeded", entry.Status, "%+v", entry)
				require.NotNil(t, entry.Plan)
				require.Len(t, entry.Plan.Mapping, 4)
				want := map[string]int64{"Low": 1, "Mid": 2, "High": 3, "Empty": 0}
				for _, mapping := range entry.Plan.Mapping {
					require.Equal(t, want[mapping.SourceName], entry.SourceRows[partition.LeafKey(mapping.SourceSchema, mapping.SourceName)])
					require.Equal(t, want[mapping.SourceName], entry.TargetRows[partition.LeafKey(mapping.TargetSchema, mapping.TargetName)])
				}
			}
			var payload, amount string
			require.NoError(t, db.QueryRowContext(ctx, "SELECT `payload`,`amount` FROM "+mysqlTable(ordinary)+" WHERE `id`=7").Scan(&payload, &amount))
			require.Equal(t, "边界 ", payload)
			require.Equal(t, "123456789.012", amount)
			views := NewMigrator(reader, writer, job, cfg).MigrateViews(ctx, []string{view})
			require.Len(t, views, 1)
			require.Empty(t, views[0].Error, "%+v", views[0])
			require.NoError(t, db.QueryRowContext(ctx, "SELECT `payload` FROM "+mysqlTable(view)+" WHERE `id`=7").Scan(&payload))
			require.Equal(t, "边界 ", payload)
			cfg.Content, cfg.Filter, cfg.CaptureRepair = "data_only", rangeTable, false
			appended := NewMigrator(reader, writer, job, cfg).Run(ctx)
			require.False(t, appended.HasFailures(), "append report=%+v", appended)
			require.Equal(t, validation.StatusPassed, appended.DataValidation.Status)
			var count int64
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mysqlTable(rangeTable)).Scan(&count))
			require.Equal(t, int64(12), count)
		})
	}
}
