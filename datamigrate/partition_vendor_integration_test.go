package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

// A product label is never certification: log the server's actual identity and
// exercise its own Writer/driver against an explicitly isolated disposable DB.
func TestMySQLVendorPartitionIntegration(t *testing.T) {
	sourceDSN, targetDSN, product := os.Getenv("DBGOLD_PARTITION_MYSQL_DSN"), os.Getenv("DBGOLD_PARTITION_VENDOR_DSN"), os.Getenv("DBGOLD_PARTITION_VENDOR_PRODUCT")
	if os.Getenv("DBGOLD_PARTITION_ALLOW_DDL") != "1" || sourceDSN == "" || targetDSN == "" || product == "" {
		t.Skip("需要独立 MySQL/厂商目标测试库和显式 DBGOLD_PARTITION_ALLOW_DDL=1；跳过不代表实库通过")
	}
	driver := os.Getenv("DBGOLD_PARTITION_VENDOR_DRIVER")
	if driver == "" {
		driver = "postgres"
		if product == "gaussdb" {
			driver = "opengauss"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	src, err := sql.Open("mysql", sourceDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open(driver, targetDSN)
	require.NoError(t, err)
	defer dst.Close()
	var database, sourceVersion, targetVersion string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT DATABASE(),VERSION()`).Scan(&database, &sourceVersion))
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT version()`).Scan(&targetVersion))
	identity := strings.ToLower(targetVersion)
	switch product {
	case "gaussdb":
		require.True(t, strings.Contains(identity, "opengauss") || strings.Contains(identity, "gaussdb"), "实际目标身份不是 Gauss/openGauss，不允许以 PG 别名代替")
	case "vastbase":
		require.Contains(t, identity, "vastbase")
	case "kingbase":
		require.Contains(t, identity, "kingbase")
	default:
		t.Fatalf("此实库夹具未实现厂商目标 %s", product)
	}
	t.Logf("source MySQL %s; writer=%s driver=%s; actual target=%s", sourceVersion, product, driver, targetVersion)
	quote := func(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
	for _, method := range []string{"range", "list"} {
		t.Run(method, func(t *testing.T) {
			name := fmt.Sprintf("dbgold_pv_%s_%x", method, time.Now().UnixNano())
			schema := name
			blocked := name + "_blocked"
			parts := []string{name + "_neg", name + "_low", name + "_high", name + "_empty"}
			_, err := dst.ExecContext(ctx, "CREATE SCHEMA "+quote(schema))
			require.NoError(t, err)
			defer dst.ExecContext(context.Background(), "DROP SCHEMA "+quote(schema)+" CASCADE")
			clause := fmt.Sprintf("PARTITION BY RANGE (`k`) (PARTITION `%s` VALUES LESS THAN (0),PARTITION `%s` VALUES LESS THAN (10),PARTITION `%s` VALUES LESS THAN (20),PARTITION `%s` VALUES LESS THAN (MAXVALUE))", parts[0], parts[1], parts[2], parts[3])
			if method == "list" {
				clause = fmt.Sprintf("PARTITION BY LIST (`k`) (PARTITION `%s` VALUES IN (-1),PARTITION `%s` VALUES IN (0,9),PARTITION `%s` VALUES IN (10,11),PARTITION `%s` VALUES IN (20))", parts[0], parts[1], parts[2], parts[3])
			}
			_, err = src.ExecContext(ctx, "CREATE TABLE `"+name+"` (`k` INT NOT NULL,`payload` VARCHAR(100)) "+clause)
			require.NoError(t, err)
			defer src.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+name+"`")
			_, err = src.ExecContext(ctx, "CREATE TABLE `"+blocked+"` (`k` INT NOT NULL) PARTITION BY KEY(`k`) PARTITIONS 2")
			require.NoError(t, err)
			defer src.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+blocked+"`")
			_, err = src.ExecContext(ctx, "INSERT INTO `"+name+"` VALUES (-1,'negative'),(0,'boundary'),(9,'nine'),(10,'ten'),(11,NULL)")
			require.NoError(t, err)
			r, err := source.NewMySQL(sourceDSN, database, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			var w target.Writer
			switch product {
			case "gaussdb":
				w, err = target.NewGaussDB(targetDSN, schema, target.ConnPoolConfig{})
			case "vastbase":
				w, err = target.NewPostgresCompatible(targetDSN, schema, product, target.ConnPoolConfig{})
			case "kingbase":
				w, err = target.NewKingbase(targetDSN, schema, target.ConnPoolConfig{})
			}
			require.NoError(t, err)
			defer w.Close()
			inspector := w.(target.PartitionInspector)
			profile, err := inspector.InspectPartitionProfile(ctx)
			require.NoError(t, err)
			t.Logf("observed product=%s version=%s mode=%s schema=%s", profile.Product, profile.Version, profile.Mode, profile.Schema)
			cfg := Config{Mode: "include", Filter: name + "," + blocked, Content: "both", TargetDBType: product, TargetSchema: schema, PageSize: 2, MaxParallel: 2, IntraTableParallel: 4, ValidateData: true, CaptureRepair: true}
			job := &Job{LogCh: make(chan string, 8192)}
			m := NewMigrator(r, w, job, cfg)
			preview := m.PreflightPartitions(ctx)
			require.False(t, preview.Ready, "unknown KEY routing is intentionally blocked")
			require.True(t, preview.CanStart, "%+v", preview.Issues)
			report := m.Run(ctx)
			require.NotNil(t, report.PartitionMigration)
			var good, bad *PartitionTableResult
			for _, table := range report.PartitionMigration.Tables {
				if table.SourceTable == name {
					good = table
				}
				if table.SourceTable == blocked {
					bad = table
				}
			}
			require.NotNil(t, good)
			require.NotNil(t, bad)
			if good.Status != "succeeded" {
				t.Fatalf("healthy partition migration failed: entry=%+v tables=%+v data=%+v validation=%+v", good, report.Tables, report.Data, report.DataValidation)
			}
			require.Equal(t, "blocked", bad.Status)
			require.Equal(t, 1, report.Data.Success)
			require.NotNil(t, report.DataValidation)
			for _, table := range report.DataValidation.Tables {
				require.Equal(t, validation.StatusPassed, table.Status, "%+v", table)
			}
			require.Len(t, report.DataValidation.Tables, 5, "validate one logical root and each of four physical leaves")
			var blockedTargets int
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2`, schema, blocked).Scan(&blockedTargets))
			require.Zero(t, blockedTargets)
			emptyKey := partition.LeafKey(schema, parts[3])
			require.Equal(t, int64(0), good.TargetRows[emptyKey])
			root := quote(schema) + "." + quote(name)
			// MySQL permits crossing leaves during UPDATE; targets must retain it.
			_, err = dst.ExecContext(ctx, "UPDATE "+root+" SET k=10 WHERE payload='negative'")
			require.NoError(t, err, "future partition-key updates must preserve source behavior")
			counts, err := inspector.CountPartitionRows(ctx, name, good.Plan.Target)
			require.NoError(t, err)
			require.Zero(t, counts[partition.LeafKey(schema, parts[0])])
			require.Equal(t, int64(3), counts[partition.LeafKey(schema, parts[2])])
			_, err = dst.ExecContext(ctx, "UPDATE "+root+" SET k=-1 WHERE payload='negative'")
			require.NoError(t, err)
			cfg.Content, cfg.ValidateData, cfg.CaptureRepair = "data_only", false, false
			appendReport := NewMigrator(r, w, job, cfg).Run(ctx)
			require.Equal(t, 1, appendReport.Data.Success, "%+v", appendReport)
			for _, table := range appendReport.PartitionMigration.Tables {
				if table.SourceTable == name {
					require.Equal(t, "succeeded", table.Status, "%+v", table)
				}
			}
			var total int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+root).Scan(&total))
			require.Equal(t, 10, total)
			_, err = dst.ExecContext(ctx, "INSERT INTO "+root+"(k,payload) VALUES(20,'future')")
			require.NoError(t, err)
			counts, err = inspector.CountPartitionRows(ctx, name, good.Plan.Target)
			require.NoError(t, err)
			require.Equal(t, int64(1), counts[emptyKey], "initially empty partition must remain usable for future writes")
		})
	}
}
