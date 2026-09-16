package datamigrate

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/driver/pgroutines"
)

// Explicit DSNs opt into disposable schemas/databases created by this fixture.
// No existing user table is read or modified. Run against each product/version.
func TestPGMySQLIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DBGOLD_PGMYSQL_SOURCE_DSN"), os.Getenv("DBGOLD_PGMYSQL_TARGET_DSN")
	if srcDSN == "" || dstDSN == "" {
		t.Skip("set DBGOLD_PGMYSQL_SOURCE_DSN and DBGOLD_PGMYSQL_TARGET_DSN for disposable integration fixtures")
	}
	product := os.Getenv("DBGOLD_PGMYSQL_PRODUCT")
	if product == "" {
		product = "postgres"
	}
	driver := os.Getenv("DBGOLD_PGMYSQL_DRIVER")
	if driver == "" {
		driver = "postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pg, e := sql.Open(driver, srcDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer pg.Close()
	my, e := sql.Open("mysql", dstDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer my.Close()
	ns := fmt.Sprintf("dbgold_it_%d", time.Now().UnixNano())
	qn := source.QuotePGIdent(ns)
	exec := func(db *sql.DB, sql string) {
		t.Helper()
		if _, e := db.ExecContext(ctx, sql); e != nil {
			t.Fatalf("%s: %v", sql, e)
		}
	}
	exec(pg, "CREATE SCHEMA "+qn)
	defer pg.ExecContext(context.Background(), "DROP SCHEMA "+qn+" CASCADE")
	exec(my, "CREATE DATABASE `"+ns+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	defer my.ExecContext(context.Background(), "DROP DATABASE `"+ns+"`")
	// serial works on older PG-derived catalogs too; modern identity has its own
	// optional fixture below for sources that advertise attidentity.
	exec(pg, `CREATE TABLE `+qn+`."Parent" ("Id" serial PRIMARY KEY, "Name" varchar(100) COLLATE "C" NOT NULL DEFAULT '' UNIQUE, "Amount" numeric(10,2) DEFAULT 0, "Active" boolean DEFAULT true, "Raw" bytea, "At" timestamptz, "Doc" jsonb, "UUID" uuid, CONSTRAINT positive_amount CHECK ("Amount">=0))`)
	exec(pg, `COMMENT ON COLUMN `+qn+`."Parent"."Name" IS 'Name''s comment'`)
	exec(pg, `COMMENT ON TABLE `+qn+`."Parent" IS 'Parent comment'`)
	exec(pg, `CREATE TABLE `+qn+`."Child" ("Id" integer PRIMARY KEY, "ParentId" integer NOT NULL, CONSTRAINT child_parent FOREIGN KEY ("ParentId") REFERENCES `+qn+`."Parent"("Id"))`)
	exec(pg, `CREATE TABLE `+qn+`."CompositeParent" ("A" integer, "B" integer, PRIMARY KEY ("B","A"))`)
	exec(pg, `CREATE TABLE `+qn+`."CompositeChild" ("Id" integer PRIMARY KEY, "X" integer, "Y" integer, CONSTRAINT paired_columns FOREIGN KEY ("X","Y") REFERENCES `+qn+`."CompositeParent"("B","A"))`)
	exec(pg, `INSERT INTO `+qn+`."CompositeParent" VALUES (4,9)`)
	exec(pg, `INSERT INTO `+qn+`."CompositeChild" VALUES (1,9,4)`)
	exec(pg, `CREATE INDEX child_parent_idx ON `+qn+`."Child"("ParentId")`)
	exec(pg, `INSERT INTO `+qn+`."Parent" ("Id","Name","Amount","Active","Raw","At","Doc","UUID") VALUES (0,'Zero',12.30,false,decode('00ff01','hex'),'2024-01-02 03:04:05.123456+08','{"a":1}','123e4567-e89b-12d3-a456-426614174000')`)
	exec(pg, `INSERT INTO `+qn+`."Parent" ("Name") VALUES ('Generated')`)
	exec(pg, `INSERT INTO `+qn+`."Child" VALUES (1,0)`)
	exec(pg, `SELECT setval(pg_get_serial_sequence('`+qn+`."Parent"','Id'),100,true)`)
	exec(pg, `CREATE TABLE `+qn+`."奇妙""表" ("select" varchar(40))`)
	exec(pg, `INSERT INTO `+qn+`."奇妙""表" VALUES ('中文与引号')`)
	exec(pg, `CREATE VIEW `+qn+`."ActiveView" AS SELECT p."Id",p."Name" FROM `+qn+`."Parent" p WHERE p."Active"=true`)
	r, e := source.NewPostgresProduct(product, driver, srcDSN, ns, source.ConnPoolConfig{})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	w, e := target.NewMySQL(dstDSN, ns, target.ConnPoolConfig{})
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	job := &Job{LogCh: make(chan string, 512)}
	cfg := Config{Mode: "all", Content: "both", TargetSchema: ns, TargetDBType: "mysql", PageSize: 1, LowerCaseNames: true}
	version, e := w.InspectMySQL(ctx)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("source=%s %s mode=%s target=%s", r.PGProfile().Product, r.PGProfile().Version, r.PGProfile().Mode, version.Version)
	if !version.Parsed.Checks() {
		cfg.ExcludedObjects = []ObjectRef{{Kind: "check", Table: "Parent", Name: "positive_amount"}}
	}
	m := NewMigrator(r, w, job, cfg)
	report := m.Run(ctx)
	if report.HasFailures() {
		t.Fatalf("report=%+v issues=%+v tables=%+v data=%+v constraints=%+v views=%+v", report, report.Preflight.Issues, report.Tables, report.Data, report.Constraints, report.Views)
	}
	if report.Tables.Success != 5 || report.Data.Success != 5 || report.Views.Success != 1 || report.Constraints.Success < 1 {
		t.Fatalf("missing objects: %+v", report)
	}
	var amount, at, uuid string
	var b []byte
	var active int
	if e = my.QueryRowContext(ctx, `SELECT amount,DATE_FORMAT(at,'%Y-%m-%d %H:%i:%s.%f'),uuid,raw,active FROM `+"`"+ns+"`.parent WHERE id=0").Scan(&amount, &at, &uuid, &b, &active); e != nil {
		t.Fatal(e)
	}
	if amount != "12.30" || at != "2024-01-01 19:04:05.123456" || hex.EncodeToString(b) != "00ff01" || active != 0 || uuid != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("values changed: %s %s %x %d %s", amount, at, b, active, uuid)
	}
	var comment string
	if e = my.QueryRowContext(ctx, `SELECT COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='parent' AND COLUMN_NAME='name'`, ns).Scan(&comment); e != nil || comment != "Name's comment" {
		t.Fatal(comment, e)
	}
	exec(my, "INSERT INTO `"+ns+"`.parent(name) VALUES ('After migration')")
	var id int
	if e = my.QueryRowContext(ctx, "SELECT id FROM `"+ns+"`.parent WHERE name='After migration'").Scan(&id); e != nil || id != 101 {
		t.Fatal(id, e)
	}
	// Re-running rebuilds internal FKs in the same plan without FK_CHECKS=0.
	report = NewMigrator(r, w, job, cfg).Run(ctx)
	if report.HasFailures() {
		t.Fatalf("repeat failed: preflight=%+v tables=%+v constraints=%+v", report.Preflight, report.Tables, report.Constraints)
	}
	// Separate schema-only and data-only runs retain existing checks/FKs and
	// load parents first, without disabling FK enforcement.
	schemaCfg := cfg
	schemaCfg.Content = "schema_only"
	schemaReport := NewMigrator(r, w, job, schemaCfg).Run(ctx)
	if schemaReport.HasFailures() {
		t.Fatalf("schema-only: %+v %+v", schemaReport.Preflight, schemaReport.Tables)
	}
	// Existing columns retain general_ci even after database defaults change.
	exec(my, "ALTER DATABASE `"+ns+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
	dataCfg := cfg
	dataCfg.Content = "data_only"
	dataReport := NewMigrator(r, w, job, dataCfg).Run(ctx)
	if dataReport.HasFailures() {
		t.Fatalf("data-only: preflight=%+v data=%+v", dataReport.Preflight, dataReport.Data)
	}
	if got := dataReport.Preflight.textRules["Parent"]["Name"].Collation; got != "utf8mb4_general_ci" {
		t.Fatalf("existing column override ignored: %s", got)
	}
	var nextAuto int64
	if e = my.QueryRowContext(ctx, `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME='parent'`, ns).Scan(&nextAuto); e != nil || nextAuto != 101 {
		t.Fatal("data-only sequence gap lost", nextAuto, e)
	}
	var unicodeText string
	if e = my.QueryRowContext(ctx, "SELECT `select` FROM `"+ns+"`.`奇妙\"表`").Scan(&unicodeText); e != nil || unicodeText != "中文与引号" {
		t.Fatal(unicodeText, e)
	}
	// Unsafe mixed expression index must stop before DROP of the existing target.
	exec(pg, `CREATE UNIQUE INDEX mixed_expression ON `+qn+`."Parent" ("Id",lower("Name"))`)
	blocked := NewMigrator(r, w, job, cfg).Run(ctx)
	if !blocked.HasFailures() || blocked.Preflight.Ready {
		t.Fatal("expression index not blocked")
	}
	var count int
	if e = my.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+ns+"`.parent").Scan(&count); e != nil || count != 2 {
		t.Fatal("blocked preflight changed target", count, e)
	}
	exec(pg, `DROP INDEX `+qn+`.mixed_expression`)
	// Standalone views use the same preflight and bypass legacy named rewrites.
	results := NewMigrator(r, w, job, cfg).MigrateViews(ctx, []string{"ActiveView"})
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("standalone views: %+v", results)
	}
	// Existing column-comment edits retain AUTO_INCREMENT/default/collation.
	objectCfg := cfg
	objectCfg.Objects = []string{"comments"}
	objectCfg.TableNames = []string{"Parent"}
	objectReport := NewMigrator(r, w, job, objectCfg).Run(ctx)
	if objectReport.HasFailures() {
		t.Fatalf("comment migration failed: %+v %+v", objectReport.Preflight, objectReport.Comments)
	}
	if version.Parsed.Expressions() {
		exec(pg, `CREATE TABLE `+qn+`."Defaults" ("Id" integer, "Text" text DEFAULT E'keep\\path::value', "At" timestamptz DEFAULT '2024-01-02 03:04:05+08', "Raw" bytea DEFAULT decode('00ff','hex'), "NowAt" timestamptz DEFAULT now())`)
		// A non-whitelisted function default blocks the entire table, preserving
		// the already populated target. It is never changed to DEFAULT NULL.
		unsafe := NewMigrator(r, w, job, cfg).Run(ctx)
		if unsafe.Preflight.Ready {
			t.Fatal("function default accepted")
		}
		exec(pg, `ALTER TABLE `+qn+`."Defaults" ALTER COLUMN "Raw" SET DEFAULT '\x00ff'::bytea`)
		if r.PGProfile().Fields["pg_attribute.attidentity"] {
			exec(pg, `CREATE TABLE `+qn+`."Identity" ("Id" integer GENERATED BY DEFAULT AS IDENTITY (START WITH 11), "Value" varchar(20))`)
			exec(pg, `INSERT INTO `+qn+`."Identity" ("Value") VALUES ('identity')`)
		}
		extended := NewMigrator(r, w, job, cfg).Run(ctx)
		if extended.HasFailures() {
			t.Fatalf("extended fixture failed: preflight=%+v tables=%+v data=%+v", extended.Preflight, extended.Tables, extended.Data)
		}
		conn, err := my.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(ctx, "SET SESSION time_zone='+08:00'"); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO `"+ns+"`.defaults() VALUES ()"); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		var utcDelta int64
		if err = conn.QueryRowContext(ctx, "SELECT ABS(TIMESTAMPDIFF(SECOND,nowat,UTC_TIMESTAMP())) FROM `"+ns+"`.defaults").Scan(&utcDelta); err != nil || utcDelta > 2 {
			conn.Close()
			t.Fatal("UTC default changed with session timezone", utcDelta, err)
		}
		conn.ExecContext(ctx, "SET SESSION time_zone='+00:00'")
		conn.Close()
		var text, at, raw string
		if e = my.QueryRowContext(ctx, "SELECT text,DATE_FORMAT(at,'%Y-%m-%d %H:%i:%s'),HEX(raw) FROM `"+ns+"`.defaults").Scan(&text, &at, &raw); e != nil || text != `keep\path::value` || at != "2024-01-01 19:04:05" || raw != "00FF" {
			t.Fatalf("default values: %q %s %s err=%v", text, at, raw, e)
		}
	}
	// Raw function definitions preserve strings/casing and a replayable terminator.
	exec(pg, `CREATE FUNCTION `+qn+`."CaseFunction"() RETURNS text LANGUAGE sql AS $$ SELECT 'Keep::Case'::text $$`)
	routines, e := pgroutines.Extract(pg, ns, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(routines) != 1 || !strings.Contains(routines[0].Body, "Keep::Case") || !strings.HasSuffix(strings.TrimSpace(routines[0].Body), ";") {
		t.Fatal(routines)
	}
}
