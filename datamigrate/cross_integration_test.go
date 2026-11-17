package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// Opt-in only: these DSNs must point to disposable dedicated databases/schemas.
// Run once per server/version/DM compatibility mode; no server settings change.
func TestCrossMigrationIntegration(t *testing.T) {
	for _, pair := range [][2]string{{"dameng", "oracle"}, {"dameng", "sqlserver"}, {"sqlserver", "oracle"}} {
		t.Run(pair[0]+"_"+pair[1], func(t *testing.T) {
			src, dst := pair[0], pair[1]
			env := func(product, key string) string {
				return os.Getenv("DBGOLD_CROSS_TEST_" + strings.ToUpper(product) + "_" + key)
			}
			srcDSN, dstDSN := env(src, "DSN"), env(dst, "DSN")
			if os.Getenv("DBGOLD_CROSS_TEST_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" {
				t.Skip("SKIP: requires explicit DBGOLD_CROSS_TEST_ALLOW_DDL=1 and both dedicated test DSNs")
			}
			srcSchema, dstSchema := env(src, "SCHEMA"), env(dst, "SCHEMA")
			require.True(t, strings.HasPrefix(dstSchema, "DBGOLD_"), "target schema must begin DBGOLD_")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			driverName := src
			if src == "dameng" {
				driverName = "dm"
			}
			srcDB, e := sql.Open(driverName, srcDSN)
			require.NoError(t, e)
			defer srcDB.Close()
			dstDB, e := sql.Open(dst, dstDSN)
			require.NoError(t, e)
			defer dstDB.Close()
			var dbName string
			if src == "sqlserver" {
				require.NoError(t, srcDB.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&dbName))
				require.True(t, strings.HasPrefix(dbName, "DBGOLD_"), "SQL Server source database must begin DBGOLD_")
				srcSchema = "dbo"
			} else {
				require.True(t, strings.HasPrefix(srcSchema, "DBGOLD_"), "DM source schema must begin DBGOLD_")
			}
			var r source.Reader
			var w target.Writer
			if src == "dameng" {
				r, e = source.NewDaMengCross(srcDSN, srcSchema, source.ConnPoolConfig{MaxOpenConns: 1})
			} else {
				r, e = source.NewSQLServerForOracle(srcDSN, dbName, source.ConnPoolConfig{MaxOpenConns: 1})
			}
			require.NoError(t, e)
			defer r.Close()
			if dst == "oracle" {
				w, e = target.NewOracle(dstDSN, dstSchema, target.ConnPoolConfig{})
			} else {
				w, e = target.NewSQLServer(dstDSN, dstSchema, target.ConnPoolConfig{})
			}
			require.NoError(t, e)
			defer w.Close()
			name := fmt.Sprintf("CX_%x", time.Now().UnixNano())
			quote := func(v string) string { return `"` + strings.ReplaceAll(v, `"`, `""`) + `"` }
			if src == "sqlserver" {
				quote = func(v string) string { return "[" + strings.ReplaceAll(v, "]", "]]") + "]" }
			}
			srcName := quote(srcSchema) + "." + quote(name)
			targetName := name
			if dst == "oracle" {
				targetName = strings.ToUpper(name)
			}
			dstName := w.Dialect().QualifyTable(dstSchema, targetName)
			created := false
			defer func() {
				clean, c := context.WithTimeout(context.Background(), 15*time.Second)
				defer c()
				if created {
					_, e := srcDB.ExecContext(clean, "DROP TABLE "+srcName)
					require.NoError(t, e)
				}
				_, _ = dstDB.ExecContext(clean, "DROP TABLE "+dstName)
				if dst == "oracle" {
					_, _ = dstDB.ExecContext(clean, "DROP SEQUENCE "+w.Dialect().QualifyTable(dstSchema, dialect.OracleObjectName("SEQ", name, "Id")))
				}
			}()
			stampType := "TIMESTAMP(7)"
			if src == "sqlserver" {
				stampType = "DATETIME2(7)"
			}
			ddl := "CREATE TABLE " + srcName + " (" + quote("Id") + " INT IDENTITY(10,5) NOT NULL, " + quote("Amount") + " DECIMAL(38,18), " + quote("Text") + " NVARCHAR(32), " + quote("At") + " " + stampType + ", CONSTRAINT " + quote("PK_"+name) + " PRIMARY KEY (" + quote("Id") + "))"
			_, e = srcDB.ExecContext(ctx, ddl)
			require.NoError(t, e)
			created = true
			stamp := "TIMESTAMP '2026-01-02 03:04:05.1234567'"
			if src == "sqlserver" {
				stamp = "CAST('2026-01-02 03:04:05.1234567' AS DATETIME2(7))"
			}
			for range 2 {
				_, e = srcDB.ExecContext(ctx, "INSERT INTO "+srcName+" ("+quote("Amount")+","+quote("Text")+","+quote("At")+") VALUES (12345678901234567890.123456789012345678,N'中文',"+stamp+")")
				require.NoError(t, e)
			}
			var ledger RepairManifest
			cfg := Config{TargetDBType: dst, TargetSchema: dstSchema, Mode: "include", Filter: name, Content: "schema_only", PageSize: 1, MaxParallel: 1, CaptureRepair: true, RepairSink: func(v RepairManifest) error { ledger = v; return nil }}
			run := func(ctx context.Context, cfg Config) MigrationReport {
				job := &Job{LogCh: make(chan string, 4096)}
				report := NewMigrator(r, w, job, cfg).Run(ctx)
				for _, issue := range report.CrossPreflight.Issues {
					t.Log(issue.Object, issue.Reason)
				}
				return report
			}
			structure := run(ctx, cfg)
			require.False(t, structure.HasFailures(), "schema report: %+v", structure)
			var evidence []source.SequenceInfo
			for _, o := range ledger.Objects {
				if o.TrustedCrossOracleSequence() {
					evidence = append(evidence, *o.Sequence)
				}
			}
			cfg.Content = "data_only"
			ctx = WithCrossOracleEvidence(ctx, evidence)
			data := run(ctx, cfg)
			require.False(t, data.HasFailures(), "data report: %+v", data)
			var count int
			require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dstName).Scan(&count))
			require.Equal(t, 2, count)
			idName, amountName, textName, atName := "Id", "Amount", "Text", "At"
			if dst == "oracle" {
				idName, amountName, textName, atName = "ID", "AMOUNT", "TEXT", "AT"
			}
			qi := w.Dialect().QuoteIdent
			expr := "CONVERT(varchar(100)," + qi(amountName) + ")," + qi(textName) + ",CONVERT(varchar(100)," + qi(atName) + ",126)"
			if dst == "oracle" {
				expr = "TO_CHAR(" + qi(amountName) + ",'FM99999999999999999999D999999999999999999','NLS_NUMERIC_CHARACTERS=''.,''')," + qi(textName) + ",TO_CHAR(" + qi(atName) + ",'YYYY-MM-DD HH24:MI:SS.FF7')"
			}
			var number, text, stampText string
			require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT "+expr+" FROM "+dstName+" WHERE "+qi(idName)+"=10").Scan(&number, &text, &stampText))
			require.Equal(t, "12345678901234567890.123456789012345678", number)
			require.Equal(t, "中文", text)
			require.Contains(t, stampText, "1234567")
			_, e = dstDB.ExecContext(ctx, "INSERT INTO "+dstName+" ("+qi(amountName)+") VALUES (1)")
			require.NoError(t, e)
			var next int64
			require.NoError(t, dstDB.QueryRowContext(ctx, "SELECT MAX("+qi(idName)+") FROM "+dstName).Scan(&next))
			require.Greater(t, next, int64(15))
			require.Zero(t, (next-10)%5)
		})
	}
}
