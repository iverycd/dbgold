package datamigrate

import (
	"bytes"
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
	"dbgold/datamigrate/validation"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Every caller supplies a fresh namespace on its dedicated DM test instance.
// Precreate dependencies of the nine compatibility routines outside the
// phase being measured, so the normal migration report can pass in full.
func createDMScanSchema(t *testing.T, admin *sql.DB, namespace string) {
	t.Helper()
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	q := quote(namespace)
	_, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+q+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := admin.ExecContext(ctx, "DROP SCHEMA "+q+" CASCADE")
		require.NoError(t, err)
	})
	for _, table := range []string{
		"FRAME_OU (OUGUID VARCHAR(50), PARENTOUGUID VARCHAR(50), OUNAME VARCHAR(50))",
		"FRAME_USER (USERGUID VARCHAR(50), OUGUID VARCHAR(50), DISPLAYNAME VARCHAR(50))",
		"CODE_MAIN (CODEID INT, CODENAME VARCHAR(50))",
		"CODE_ITEMS (CODEID INT, ITEMVALUE VARCHAR(50), ITEMTEXT VARCHAR(50))",
		"API_MANAGE_CONFIG (APIGUID VARCHAR(50))",
		"CODE_MAXNUMBER (NUMBERNAME VARCHAR(50), CURRENTVALUE VARCHAR(50), NUMBERFLAG VARCHAR(50))",
	} {
		_, err := admin.ExecContext(t.Context(), "CREATE TABLE "+q+"."+table)
		require.NoError(t, err, table)
	}
}

func TestMySQLDMBatchIntegration(t *testing.T) {
	myDSN, dmDSN := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQL_SCAN_DM_DSN")
	if myDSN == "" || dmDSN == "" {
		t.Skip("requires disposable DBGOLD_MYSQL_SCAN_SOURCE_DSN and DBGOLD_MYSQL_SCAN_DM_DSN")
	}
	open := func(driver, dsn string) *sql.DB {
		t.Helper()
		db, err := sql.Open(driver, dsn)
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		return db
	}
	ctx := t.Context()
	myAdmin := open("mysql", myDSN)
	dm := open("dm", dmDSN)
	sourceDB := fmt.Sprintf("dbgold_dm_src_%d", time.Now().UnixNano())
	_, err := myAdmin.ExecContext(ctx, "CREATE DATABASE "+sourceDB+" CHARACTER SET utf8mb4")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := myAdmin.Exec("DROP DATABASE " + sourceDB)
		require.NoError(t, err)
	})
	cfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	cfg.DBName = sourceDB
	my := open("mysql", cfg.FormatDSN())
	my.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := my.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	exec("CREATE TABLE AutoMixed (Id BIGINT AUTO_INCREMENT PRIMARY KEY, N INT, T VARCHAR(100), At DATETIME(6), Amount DECIMAL(28,6))")
	exec("INSERT INTO AutoMixed VALUES (2,NULL,NULL,NULL,NULL),(5,17,'甲','2026-03-01 12:15:07.150002',123456789012345678.123456),(9,19,'多字节','2026-03-01 12:15:07.000007',-123.456789),(13,23,'tail',NULL,0)")
	exec("CREATE TABLE Compound (Part INT, Seq BIGINT UNSIGNED, Payload VARCHAR(100), PRIMARY KEY(Part, Seq))")
	exec("INSERT INTO Compound VALUES (1,9,'a'),(2,9,'b'),(1,9223372036854775807,'边界'),(2,17,'d')")
	exec("CREATE TABLE WideMixed (Id INT PRIMARY KEY, Txt LONGTEXT, Bin MEDIUMBLOB)")
	binary := bytes.Repeat([]byte{0, 255, 0, 128}, 3000)
	exec("INSERT INTO WideMixed VALUES (1,NULL,NULL),(2,'tiny',X'FF0001'),(3,?,?),(4,'tail',X'EEFF'),(5,'five',X'0088')", strings.Repeat("中文", 2700), binary)
	exec("CREATE TABLE ZeroDates (Id INT PRIMARY KEY, D DATE, At DATETIME(6))")
	exec("SET SESSION sql_mode=''")
	exec("INSERT INTO ZeroDates VALUES (1,'0000-00-00','2024-12-00 12:00:00'),(2,'2024-02-29','2024-02-29 01:02:03.123456')")

	for _, strategy := range []string{"row", "batch"} {
		t.Run(strategy, func(t *testing.T) {
			ns := fmt.Sprintf("DM_BATCH_CHECK_%d", time.Now().UnixNano())
			createDMScanSchema(t, dm, ns)
			reader, err := source.NewMySQLForDameng(cfg.FormatDSN(), sourceDB, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer reader.Close()
			writer, err := target.NewDaMeng(dmDSN, ns, target.ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
			require.NoError(t, err)
			defer writer.Close()
			run := func(filter string) MigrationReport {
				job := &Job{LogCh: make(chan string, 4096)}
				m := NewMigrator(reader, writer, job, Config{Mode: "include", Filter: filter, Content: "both", TargetSchema: ns,
					TargetDBType: "dameng", MaxParallel: 2, IntraTableParallel: 2, PageSize: 4, ValidateData: true,
					MySQLReadStrategy: "stream", DamengWriteStrategy: strategy})
				report := m.Run(ctx)
				t.Logf("report data=%+v validation=%+v", report.Data, report.DataValidation)
				return report
			}
			report := run("AutoMixed,Compound,WideMixed")
			require.False(t, report.HasFailures(), "tables=%+v data=%+v content=%+v routines=%+v", report.Tables, report.Data, report.DataValidation, report.CompatibilityFunctions)
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			require.Len(t, report.DataValidation.Tables, 3)
			var explicitCount int
			require.NoError(t, dm.QueryRow(`SELECT COUNT(*) FROM "`+ns+`"."AUTOMIXED" WHERE "ID" IN (2,5,9,13)`).Scan(&explicitCount))
			require.Equal(t, 4, explicitCount)
			_, err = dm.Exec(`INSERT INTO "` + ns + `"."AUTOMIXED" ("N") VALUES(91)`)
			require.NoError(t, err)
			var generated int
			require.NoError(t, dm.QueryRow(`SELECT "ID" FROM "`+ns+`"."AUTOMIXED" WHERE "N"=91`).Scan(&generated))
			require.Greater(t, generated, 13)
			bulks, scalar := writer.BatchWriteStats()
			if strategy == "batch" {
				require.Positive(t, bulks)
			} else {
				require.Zero(t, bulks)
			}
			require.Positive(t, scalar)

			zero := run("ZeroDates")
			require.Equal(t, 1, zero.Data.Success)
			require.True(t, zero.HasFailures(), "zero date replacement must remain observable to raw validation")
			require.Equal(t, validation.StatusMismatch, zero.DataValidation.Status)
			var d, dt string
			require.NoError(t, dm.QueryRow(`SELECT TO_CHAR("D",'YYYY-MM-DD'),TO_CHAR("AT",'YYYY-MM-DD HH24:MI:SS') FROM "`+ns+`"."ZERODATES" WHERE "ID"=1`).Scan(&d, &dt))
			require.Equal(t, "1900-01-01", d)
			require.Equal(t, "1900-01-01 00:00:00", dt)
		})
	}
}

// The first page commits. A later page's second array chunk violates CHECK;
// the first chunk of that same page must disappear and the next table must load.
func TestMySQLDMBatchFailureIntegration(t *testing.T) {
	myDSN, dmDSN := os.Getenv("DBGOLD_MYSQL_SCAN_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQL_SCAN_DM_DSN")
	if myDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM test instances")
	}
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	dm, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	defer dm.Close()
	ns := fmt.Sprintf("dbgold_dm_fail_%d", time.Now().UnixNano())
	exec := func(db *sql.DB, sql string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), sql)
		require.NoError(t, err, sql)
	}
	exec(my, "CREATE DATABASE "+ns)
	defer exec(my, "DROP DATABASE "+ns)
	exec(my, "CREATE TABLE "+ns+".AAA_REJECT (ID BIGINT AUTO_INCREMENT PRIMARY KEY, TAG INT NOT NULL)")
	exec(my, "CREATE TABLE "+ns+".ZZZ_GOOD LIKE "+ns+".AAA_REJECT")
	values := make([]string, 2010)
	for i := range values {
		values[i] = fmt.Sprintf("(%d,%d)", i+1, i+1)
	}
	exec(my, "INSERT INTO "+ns+".AAA_REJECT VALUES "+strings.Join(values, ","))
	exec(my, "INSERT INTO "+ns+".ZZZ_GOOD VALUES (17,17),(18,18),(19,19)")
	cfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	cfg.DBName = ns
	for _, strategy := range []string{"row", "batch"} {
		t.Run(strategy, func(t *testing.T) {
			schema := fmt.Sprintf("DM_CHK_FAIL_%d", time.Now().UnixNano())
			createDMScanSchema(t, dm, schema)
			bad := dialect.NewDaMeng().QualifyTable(schema, "AAA_REJECT")
			good := dialect.NewDaMeng().QualifyTable(schema, "ZZZ_GOOD")
			exec(dm, "CREATE TABLE "+bad+" (ID BIGINT IDENTITY(1,1) PRIMARY KEY, TAG INT NOT NULL, CONSTRAINT SCAN_REJECT_2005 CHECK (TAG <> 2005))")
			exec(dm, "CREATE TABLE "+good+" (ID BIGINT IDENTITY(1,1) PRIMARY KEY, TAG INT NOT NULL)")
			r, err := source.NewMySQLForDameng(cfg.FormatDSN(), ns, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewDaMeng(dmDSN, schema, target.ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
			require.NoError(t, err)
			defer w.Close()
			report := NewMigrator(r, w, &Job{LogCh: make(chan string, 2048)}, Config{Mode: "all", Content: "data_only", TargetSchema: schema, TargetDBType: "dameng", MaxParallel: 1, IntraTableParallel: 1, PageSize: 1003, MySQLReadStrategy: "stream", DamengWriteStrategy: strategy}).Run(t.Context())
			require.Equal(t, 1, report.Data.Success, "%+v", report.Data)
			require.Equal(t, 1, report.Data.Failed, "%+v", report.Data)
			require.Len(t, report.Data.Items, 1)
			require.Equal(t, "AAA_REJECT", report.Data.Items[0].Name)
			require.Contains(t, strings.ToUpper(report.Data.Items[0].Error), "SCAN_REJECT_2005")
			var count, first, last int
			require.NoError(t, dm.QueryRow("SELECT COUNT(*), MIN(ID), MAX(ID) FROM "+bad).Scan(&count, &first, &last))
			require.Equal(t, []int{1003, 1, 1003}, []int{count, first, last})
			require.NoError(t, dm.QueryRow("SELECT COUNT(*) FROM "+good+" WHERE TAG=ID AND ID IN (17,18,19)").Scan(&count))
			require.Equal(t, 3, count)
			require.Len(t, report.RowCounts, 1)
			require.True(t, report.RowCounts[0].Match)
			bulk, scalar := w.BatchWriteStats()
			t.Logf("%s: 1003 earlier rows retained, entire failed page rolled back, next independent table copied; bulk=%d scalar=%d", strategy, bulk, scalar)
		})
	}
}
