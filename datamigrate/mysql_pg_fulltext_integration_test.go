package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// The DSNs must point to disposable servers. Each run creates its own MySQL
// database and PostgreSQL schema.
func TestMySQLPGFulltextIntegration(t *testing.T) {
	sourceDSN := os.Getenv("DBGOLD_MYSQLPG_FULLTEXT_SOURCE_DSN")
	targetDSN := os.Getenv("DBGOLD_MYSQLPG_FULLTEXT_TARGET_DSN")
	if sourceDSN == "" || targetDSN == "" {
		t.Skip("set DBGOLD_MYSQLPG_FULLTEXT_SOURCE_DSN and DBGOLD_MYSQLPG_FULLTEXT_TARGET_DSN for disposable MySQL and PostgreSQL servers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	my, err := sql.Open("mysql", sourceDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = my.Close() })
	pg, err := sql.Open("postgres", targetDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	var srcVersion, dstVersion string
	require.NoError(t, my.QueryRowContext(ctx, "SELECT VERSION()").Scan(&srcVersion))
	require.NoError(t, pg.QueryRowContext(ctx, "SELECT version()").Scan(&dstVersion))
	t.Logf("source=%s target=%s", srcVersion, dstVersion)

	namespace := fmt.Sprintf("dbgold_fulltext_%x", time.Now().UnixNano())
	myTable := func(name string) string { return "`" + namespace + "`.`" + name + "`" }
	pgName := func(name string) string { return `"` + namespace + `".` + source.QuotePGIdent(name) }
	exec := func(t *testing.T, db *sql.DB, query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err, "%s", query)
	}
	cleanup := func(db *sql.DB, query string) {
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			_, err := db.ExecContext(cleanupCtx, query)
			require.NoError(t, err)
		})
	}
	exec(t, my, "CREATE DATABASE `"+namespace+"` CHARACTER SET utf8mb4")
	cleanup(my, "DROP DATABASE `"+namespace+"`")
	exec(t, pg, "CREATE SCHEMA "+source.QuotePGIdent(namespace))
	cleanup(pg, "DROP SCHEMA "+source.QuotePGIdent(namespace)+" CASCADE")
	srcConfig, err := mysql.ParseDSN(sourceDSN)
	require.NoError(t, err)
	srcConfig.DBName = namespace
	r, err := source.NewMySQL(srcConfig.FormatDSN(), namespace, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	w, err := target.NewPostgres(targetDSN, namespace, target.ConnPoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	run := func(table string, indexesOnly, lowerCase bool) MigrationReport {
		cfg := Config{Mode: "include", Filter: table, Content: "both", TargetDBType: "postgres", TargetSchema: namespace,
			LowerCaseNames: lowerCase, PageSize: 10, MaxParallel: 1, CaptureRepair: true}
		if indexesOnly {
			cfg.Content, cfg.Objects, cfg.TableNames = "schema_only", []string{"indexes"}, []string{table}
		}
		return NewMigrator(r, w, &Job{LogCh: make(chan string, 1024)}, cfg).Run(ctx)
	}
	findIndex := func(t *testing.T, report MigrationReport, name string) repair.Object {
		t.Helper()
		require.NotNil(t, report.RepairManifest)
		for _, obj := range report.RepairManifest.Objects {
			if obj.Kind == "index" && obj.SourceName == name {
				return obj
			}
		}
		t.Fatalf("repair manifest missing index %q", name)
		return repair.Object{}
	}
	verifyIndex := func(t *testing.T, obj repair.Object) {
		t.Helper()
		verified, err := target.InspectRepair(ctx, w, obj)
		require.NoError(t, err)
		require.Equal(t, "succeeded", verified.Status, "%+v", verified)
	}
	indexDefinition := func(t *testing.T, table, index string) string {
		t.Helper()
		var definition string
		require.NoError(t, pg.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=$1 AND tablename=$2 AND indexname=$3`,
			namespace, table, index).Scan(&definition))
		return definition
	}

	t.Run("composite ngram and indexes-only recovery", func(t *testing.T) {
		const table = "frame_attachstorage"
		const originalIndex = "I_FRAME_ATTACHSTORAGE_CGCT"
		const index = "i_frame_attachstorage_cgct"
		const originalNgram = "Z_NGRAM"
		const ngram = "z_ngram"
		const plain = "zz_plain"
		exec(t, my, "CREATE TABLE "+myTable(table)+" (`Id` INT PRIMARY KEY, `CLIENGTAG` VARCHAR(100), `CLIENGGUID` TEXT, `Payload` VARCHAR(100), "+
			"FULLTEXT KEY `"+originalIndex+"` (`CLIENGTAG`,`CLIENGGUID`), FULLTEXT KEY `"+originalNgram+"` (`CLIENGGUID`) WITH PARSER ngram, "+
			"KEY `ZZ_PLAIN` (`Payload`)) ENGINE=InnoDB")
		exec(t, my, "INSERT INTO "+myTable(table)+" VALUES (1,'alphatag',NULL,'one'),(2,NULL,'betaguid','two'),(3,NULL,NULL,'three')")

		report := run(table, false, true)
		require.False(t, report.HasFailures(), "%+v", report)
		require.Equal(t, 1, report.Data.Success)
		require.Equal(t, 3, report.Indexes.Success)
		obj := findIndex(t, report, originalIndex)
		require.Equal(t, index, obj.TargetName)
		require.NotNil(t, obj.Index)
		require.Equal(t, []string{"cliengtag", "cliengguid"}, obj.Index.Columns)
		require.Contains(t, obj.DDL, "USING gin")
		require.Contains(t, obj.DDL, "pg_catalog.simple")
		require.Contains(t, indexDefinition(t, table, index), "USING gin")
		verifyIndex(t, obj)
		ngramObj := findIndex(t, report, originalNgram)
		require.Contains(t, ngramObj.DDL, "pg_catalog.simple")
		verifyIndex(t, ngramObj)
		var fulltextWarnings int
		for _, warning := range report.Warnings {
			if strings.Contains(warning.Reason, "MATCH") && strings.Contains(warning.Reason, "AGAINST") {
				require.Contains(t, warning.Reason, "simple")
				fulltextWarnings++
			}
		}
		require.Equal(t, 2, fulltextWarnings, "%+v", report.Warnings)

		const expression = `pg_catalog.to_tsvector('pg_catalog.simple'::pg_catalog.regconfig, COALESCE("cliengtag"::pg_catalog.text, ''::pg_catalog.text) || ' '::pg_catalog.text || COALESCE("cliengguid"::pg_catalog.text, ''::pg_catalog.text))`
		predicate := expression + ` @@ pg_catalog.plainto_tsquery('pg_catalog.simple'::pg_catalog.regconfig, $1)`
		for term, expectedID := range map[string]int{"alphatag": 1, "betaguid": 2} {
			var actualID, count int
			require.NoError(t, pg.QueryRowContext(ctx, "SELECT MIN(id), COUNT(*) FROM "+pgName(table)+" WHERE "+predicate, term).Scan(&actualID, &count))
			require.Equal(t, expectedID, actualID)
			require.Equal(t, 1, count)
		}
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off")
		require.NoError(t, err)
		var plan string
		require.NoError(t, tx.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) SELECT id FROM "+pgName(table)+" WHERE "+predicate, "alphatag").Scan(&plan))
		require.Contains(t, plan, index)
		require.NoError(t, tx.Rollback())

		// A row added on the destination makes unintended data recopy or table
		// replacement observable during the object-only retries.
		exec(t, pg, "INSERT INTO "+pgName(table)+" (id) VALUES (4)")
		report = run(table, true, true)
		require.Zero(t, report.Data.Total)
		require.Zero(t, report.Tables.Success)
		require.Zero(t, report.Indexes.Failed, "%+v", report.Indexes)
		require.Equal(t, 3, report.Indexes.Success)

		exec(t, pg, "DROP INDEX "+pgName(index))
		// This is a valid GIN index with the same name, table and columns but
		// a different key expression. Exact verification must reject it.
		exec(t, pg, "CREATE INDEX "+source.QuotePGIdent(index)+" ON "+pgName(table)+
			` USING gin (pg_catalog.to_tsvector('pg_catalog.simple'::pg_catalog.regconfig, COALESCE(cliengguid, '') || ' ' || COALESCE(cliengtag::text, '')))`)
		wrong := indexDefinition(t, table, index)
		exec(t, pg, "DROP INDEX "+pgName(ngram))
		exec(t, pg, "DROP INDEX "+pgName(plain))
		report = run(table, true, true)
		require.Equal(t, 2, report.Indexes.Success, "%+v", report.Indexes)
		require.Equal(t, 1, report.Indexes.Failed, "%+v", report.Indexes)
		require.Len(t, report.Indexes.Items, 1)
		failure := report.Indexes.Items[0]
		require.Equal(t, originalIndex, failure.SourceName)
		require.Equal(t, "execute", failure.Stage)
		require.Equal(t, obj.DDL, failure.DDL)
		require.NotEmpty(t, failure.Error)
		require.Equal(t, wrong, indexDefinition(t, table, index))
		require.Contains(t, indexDefinition(t, table, ngram), "USING gin")
		require.Contains(t, indexDefinition(t, table, plain), "USING btree")
		var count int
		require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+pgName(table)).Scan(&count))
		require.Equal(t, 4, count)

		failed := findIndex(t, report, originalIndex)
		require.Equal(t, failure.DDL, failed.DDL)
		verified, err := target.InspectRepair(ctx, w, failed)
		require.NoError(t, err)
		require.NotEqual(t, "succeeded", verified.Status)
		require.Len(t, failed.Units, 1)
		failed.Units[0].Selected = true
		exec(t, pg, "DROP INDEX "+pgName(index))
		for attempt := 0; attempt < 2; attempt++ {
			units, err := target.ExecuteRepair(ctx, w, failed, failed.Units, nil)
			require.NoError(t, err, "repair attempt %d", attempt+1)
			require.Equal(t, "succeeded", units[0].Status)
		}
		verifyIndex(t, failed)

		exec(t, pg, "DROP INDEX "+pgName(index))
		exec(t, pg, "CREATE INDEX "+source.QuotePGIdent(index)+" ON "+pgName(table)+
			` USING gin (pg_catalog.to_tsvector('pg_catalog.simple'::pg_catalog.regconfig, COALESCE(cliengtag::text, '') || (' ' || COALESCE(cliengguid, ''))))`)
		require.Error(t, w.CreateIndex(ctx, *obj.Index), "a different expression tree cannot serve the generated full-text query")
	})

	t.Run("mixed case and quoted identifiers", func(t *testing.T) {
		const table, index = "MixedCase", `FT"Quoted`
		exec(t, my, "CREATE TABLE "+myTable(table)+" (`Body\"Text` TEXT, FULLTEXT KEY `"+index+"` (`Body\"Text`)) ENGINE=InnoDB")
		exec(t, pg, "CREATE TABLE "+pgName(table)+` ("Body""Text" text)`)
		for attempt := 0; attempt < 2; attempt++ {
			report := run(table, true, false)
			require.Zero(t, report.Indexes.Failed, "%+v", report.Indexes)
			require.Equal(t, 1, report.Indexes.Success)
			obj := findIndex(t, report, index)
			require.Equal(t, index, obj.TargetName)
			require.Contains(t, obj.DDL, `"FT""Quoted"`)
			require.Contains(t, obj.DDL, `"Body""Text"`)
			verifyIndex(t, obj)
		}
	})
}
