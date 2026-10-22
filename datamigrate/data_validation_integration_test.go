package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// These DSNs must refer to disposable instances. Tests create fresh namespaces
// only and drop them after each run; business schemas are never reused.
func contentIntegrationDatabases(t *testing.T) (*sql.DB, *sql.DB, string, string) {
	t.Helper()
	myDSN, pgDSN := os.Getenv("DBGOLD_VALIDATION_MYSQL_DSN"), os.Getenv("DBGOLD_VALIDATION_PG_DSN")
	if myDSN == "" || pgDSN == "" {
		t.Skip("set DBGOLD_VALIDATION_MYSQL_DSN and DBGOLD_VALIDATION_PG_DSN for isolated content validation fixtures")
	}
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = my.Close() })
	pg, err := sql.Open("postgres", pgDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	return my, pg, myDSN, pgDSN
}

func contentFixtureExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.Exec(query, args...)
	require.NoError(t, err, query)
}

type corruptContentPGWriter struct {
	*target.PostgresWriter
	db      *sql.DB
	schema  string
	corrupt bool
}

func (w *corruptContentPGWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	if err := w.PostgresWriter.CopyData(ctx, table, cols, types, rows); err != nil {
		return err
	}
	if w.corrupt && table == "mixed" {
		_, err := w.db.ExecContext(ctx, `UPDATE "`+w.schema+`"."mixed" SET "amount"=amount+1 WHERE "id"=1`)
		return err
	}
	return nil
}

func TestContentValidationMySQLPostgresIntegration(t *testing.T) {
	my, pg, myDSN, pgDSN := contentIntegrationDatabases(t)
	for _, scenario := range []string{"passed", "changed_same_count", "source_nul_cleaned", "timestamp_non_utc"} {
		t.Run(scenario, func(t *testing.T) {
			ns := fmt.Sprintf("dbgold_content_%d", time.Now().UnixNano())
			contentFixtureExec(t, my, "CREATE DATABASE `"+ns+"` CHARACTER SET utf8mb4")
			contentFixtureExec(t, pg, `CREATE SCHEMA "`+ns+`"`)
			t.Cleanup(func() {
				_, _ = my.Exec("DROP DATABASE `" + ns + "`")
				_, _ = pg.Exec(`DROP SCHEMA "` + ns + `" CASCADE`)
			})
			stampType := "datetime(6)"
			if scenario == "timestamp_non_utc" {
				stampType = "timestamp(6)"
			}
			contentFixtureExec(t, my, "CREATE TABLE `"+ns+"`.Mixed (Id int PRIMARY KEY, Amount decimal(30,8), Stamp "+stampType+", Label varchar(100), RawData varbinary(10), Doc json, Flags bit(1))")
			label := "保留尾空格  "
			if scenario == "source_nul_cleaned" {
				label = "a\x00b"
			}
			contentFixtureExec(t, my, "INSERT INTO `"+ns+"`.Mixed VALUES (1,12345678901234567890.12345678,'2026-10-22 12:34:56.123456',?,?,?,b'1'),(2,NULL,NULL,NULL,NULL,NULL,NULL)", label, []byte{0, 255, 1}, `{"b":[1,2],"a":1.00}`)
			contentFixtureExec(t, my, "CREATE TABLE `"+ns+"`.Keyless (value int)")
			contentFixtureExec(t, my, "INSERT INTO `"+ns+"`.Keyless VALUES (1),(1),(2)")
			contentFixtureExec(t, my, "CREATE TABLE `"+ns+"`.EmptyRows (id int)")
			cfg, err := mysql.ParseDSN(myDSN)
			require.NoError(t, err)
			cfg.DBName = ns
			cfg.ParseTime = true
			if scenario == "timestamp_non_utc" {
				if cfg.Params == nil {
					cfg.Params = map[string]string{}
				}
				cfg.Params["time_zone"] = "'+08:00'"
			}
			r, err := source.NewMySQL(cfg.FormatDSN(), ns, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			base, err := target.NewPostgres(pgDSN, ns, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer base.Close()
			w := &corruptContentPGWriter{PostgresWriter: base, db: pg, schema: ns, corrupt: scenario == "changed_same_count"}
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, Config{Mode: "all", Content: "both", PageSize: 1, MaxParallel: 1, LowerCaseNames: true, TargetSchema: ns, TargetDBType: "postgres", ValidateData: true})
			report := m.Run(context.Background())
			require.NotNil(t, report.DataValidation)
			require.Len(t, report.DataValidation.Tables, 3)
			require.Zero(t, report.Data.Failed, "%+v", report.Data.Items)
			for _, rc := range report.RowCounts {
				require.True(t, rc.Match, "%+v", rc)
			}
			if scenario == "passed" || scenario == "timestamp_non_utc" {
				require.False(t, report.HasFailures(), "%+v validation=%+v", report, report.DataValidation)
				require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			} else {
				require.Equal(t, validation.StatusMismatch, report.DataValidation.Status, "%+v", report.DataValidation)
				require.True(t, report.HasFailures())
			}
		})
	}
}

func TestContentValidationPostgresMySQLIntegration(t *testing.T) {
	my, pg, myDSN, pgDSN := contentIntegrationDatabases(t)
	ns := fmt.Sprintf("dbgold_content_%d", time.Now().UnixNano())
	contentFixtureExec(t, my, "CREATE DATABASE `"+ns+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
	contentFixtureExec(t, pg, `CREATE SCHEMA "`+ns+`"`)
	t.Cleanup(func() {
		_, _ = my.Exec("DROP DATABASE `" + ns + "`")
		_, _ = pg.Exec(`DROP SCHEMA "` + ns + `" CASCADE`)
	})
	contentFixtureExec(t, pg, `CREATE TABLE "`+ns+`"."Mixed" ("Id" integer PRIMARY KEY, "Amount" numeric(12,2), "Stamp" timestamp(6), "Label" varchar(100), "RawData" bytea, "Enabled" boolean)`)
	contentFixtureExec(t, pg, `INSERT INTO "`+ns+`"."Mixed" VALUES (1,12.30,'2026-10-22 12:34:56.123456','中文尾空格  ',$1,true),(2,NULL,NULL,NULL,NULL,NULL)`, []byte{0, 255, 1})
	r, err := source.NewPostgres(pgDSN, ns, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewMySQL(myDSN, ns, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, Config{Mode: "all", Content: "both", PageSize: 1, MaxParallel: 1, LowerCaseNames: true, TargetSchema: ns, TargetDBType: "mysql", ValidateData: true})
	report := m.Run(context.Background())
	require.NotNil(t, report.DataValidation)
	require.NotNil(t, report.Preflight)
	require.True(t, report.Preflight.Ready, "%+v", report.Preflight.Issues)
	require.False(t, report.HasFailures(), "report=%+v validation=%+v", report, report.DataValidation)
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	require.EqualValues(t, 2, report.DataValidation.Tables[0].SourceRows)
}
