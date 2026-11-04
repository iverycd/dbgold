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
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Each DSN points to a dedicated fixture database. For MySQL replay the
// destination must be a separate database from the source.
func TestMySQLPrefixIndexesIntegration(t *testing.T) {
	srcDSN := os.Getenv("MYSQL_DM_TEST_SOURCE_DSN")
	if srcDSN == "" {
		t.Skip("需要独立 MySQL 测试源库")
	}
	srcCfg, err := mysql.ParseDSN(srcDSN)
	require.NoError(t, err)
	require.NotEmpty(t, srcCfg.DBName)
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	defer src.Close()

	for _, tc := range []struct{ product, driver, env string }{
		{"dameng", "dm", "MYSQL_DM_TEST_TARGET_DSN"},
		{"mysql", "mysql", "MYSQL_PREFIX_TEST_MYSQL_DSN"},
		{"postgres", "postgres", "MYSQL_PREFIX_TEST_PG_DSN"},
	} {
		t.Run(tc.product, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip("需要 " + tc.env)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			var w target.Writer
			var r source.Reader
			var err error
			schema := "public"
			switch tc.product {
			case "dameng":
				schema = os.Getenv("MYSQL_DM_TEST_SCHEMA")
				if schema == "" {
					t.Skip("需要 MYSQL_DM_TEST_SCHEMA")
				}
				w, err = target.NewDaMeng(dsn, schema, target.ConnPoolConfig{})
			case "mysql":
				cfg, e := mysql.ParseDSN(dsn)
				require.NoError(t, e)
				schema = cfg.DBName
				require.NotEmpty(t, schema)
				require.NotEqual(t, srcCfg.DBName, schema)
				w, err = target.NewMySQL(dsn, schema, target.ConnPoolConfig{})
			case "postgres":
				w, err = target.NewPostgres(dsn, schema, target.ConnPoolConfig{})
			}
			require.NoError(t, err)
			defer w.Close()
			if tc.product == "dameng" {
				r, err = source.NewMySQLForDameng(srcDSN, srcCfg.DBName, source.ConnPoolConfig{})
			} else {
				r, err = source.NewMySQL(srcDSN, srcCfg.DBName, source.ConnPoolConfig{})
			}
			require.NoError(t, err)
			defer r.Close()
			db, err := sql.Open(tc.driver, dsn)
			require.NoError(t, err)
			defer db.Close()
			d := w.Dialect()
			base := fmt.Sprintf("audit_catalog_%d", time.Now().UnixNano())
			tables := []string{base, base + "_copy"}
			for i, table := range tables {
				srcTable := "`" + table + "`"
				unique := ""
				if i == 1 {
					unique = "UNIQUE "
				}
				_, err = src.ExecContext(ctx, "CREATE TABLE "+srcTable+" (Id INT PRIMARY KEY, RowGuid VARCHAR(36) NOT NULL, REPORT_SIGN INT NOT NULL, "+
					unique+"KEY catalog_rowguid_index (RowGuid(8), REPORT_SIGN) USING BTREE)")
				require.NoError(t, err)
				defer src.ExecContext(context.Background(), "DROP TABLE "+srcTable)
				defer db.ExecContext(context.Background(), "DROP TABLE "+d.QualifyTable(schema, table))
				_, err = src.ExecContext(ctx, "INSERT INTO "+srcTable+" VALUES (1, 'abcdefgh-one', 2)")
				require.NoError(t, err)
			}
			cfg := Config{Mode: "include", Filter: strings.Join(tables, ","), Content: "both",
				TargetDBType: tc.product, TargetSchema: schema, PageSize: 10, MaxParallel: 1, CaptureRepair: true}
			for _, mode := range []string{"both", "indexes_only"} {
				if mode == "indexes_only" {
					cfg.Content, cfg.Objects = "schema_only", []string{"indexes"}
				}
				report := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, cfg).Run(ctx)
				if tc.product == "dameng" {
					defer cleanupMySQLDMFixtureFunctions(t, db, report)
					requireMySQLDMFixtureReport(t, report, mode == "both")
				} else {
					require.False(t, report.HasFailures(), "%+v", report)
				}
				require.Equal(t, 2, report.Indexes.Success, "%+v", report.Indexes)
				var warnings int
				for _, warning := range report.Warnings {
					if strings.Contains(warning.Reason, "按整列创建") {
						warnings++
					}
				}
				if tc.product == "mysql" {
					require.Zero(t, warnings)
				} else {
					require.Equal(t, 2, warnings)
				}
				require.NotNil(t, report.RepairManifest)
				verified := map[string]bool{}
				for _, obj := range report.RepairManifest.Objects {
					if obj.Kind != "index" {
						continue
					}
					require.Equal(t, "succeeded", obj.Status)
					require.Equal(t, []int{8, 0}, obj.Index.MySQLPrefixLengths)
					require.Equal(t, "catalog_rowguid_index", obj.SourceName)
					check, err := target.InspectRepair(ctx, w, obj)
					require.NoError(t, err)
					require.Equal(t, "succeeded", check.Status, "%+v, %s", check, obj.DDL)
					verified[obj.SourceTable] = true
					if tc.product == "postgres" && mode == "indexes_only" {
						require.NoError(t, w.CreateIndex(ctx, *obj.Index), "a verified repeat is idempotent")
						wrong := *obj.Index
						wrong.TableName = tables[0]
						if obj.Table == tables[0] {
							wrong.TableName = tables[1]
						}
						require.Error(t, w.CreateIndex(ctx, wrong), "same schema name on the wrong table cannot be reused")
					}
					if mode == "both" {
						drop := "DROP INDEX " + d.QualifyTable(schema, obj.TargetName)
						if tc.product == "mysql" {
							drop = "DROP INDEX " + d.QuoteIdent(obj.TargetName) + " ON " + d.QualifyTable(schema, obj.Table)
						}
						_, err = db.ExecContext(ctx, drop)
						require.NoError(t, err)
					}
				}
				require.True(t, verified[tables[0]] && verified[tables[1]])
			}
			table := d.QualifyTable(schema, tables[1])
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
			require.Equal(t, 1, count, "indexes-only must leave the copied data intact")
			_, err = db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (2, 'abcdefgh-two', 2)")
			if tc.product == "mysql" {
				require.Error(t, err, "native UNIQUE prefix rejects equal prefixes")
			} else {
				require.NoError(t, err, "cross-database UNIQUE key compares whole values")
			}
			_, err = db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (3, 'abcdefgh-one', 2)")
			require.Error(t, err, "complete duplicate keys must always be rejected")
		})
	}
}
