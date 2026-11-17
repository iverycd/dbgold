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
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

const sqlServerSourceFixtureAmount = "12345678901234567890.123456789012345678"

func openSQLServerSourceFixture(t *testing.T, ctx context.Context) (*sql.DB, *target.SQLServerWriter, string, int) {
	t.Helper()
	dsn, schema := os.Getenv("DBGOLD_SQLSERVER_TEST_DSN"), os.Getenv("DBGOLD_SQLSERVER_TEST_SCHEMA")
	if dsn == "" || schema == "" || os.Getenv("DBGOLD_SQLSERVER_TEST_ALLOW_DDL") != "1" {
		t.Skip("requires dedicated SQL Server test DSN/schema and DBGOLD_SQLSERVER_TEST_ALLOW_DDL=1")
	}
	require.True(t, strings.HasPrefix(schema, "DBGOLD_"), "target schema must be dedicated and start with DBGOLD_")
	db, err := sql.Open("sqlserver", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var existing int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.objects WHERE schema_id=SCHEMA_ID(@p1)`, schema).Scan(&existing))
	require.Zero(t, existing, "target test schema must initially be empty")
	writer, err := target.NewSQLServer(dsn, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	major, minor, err := writer.SQLServerVersion(ctx)
	require.NoError(t, err)
	t.Logf("SQL Server actual version %d.%d", major, minor)
	return db, writer, schema, major
}

func execSQLServerSourceFixture(t *testing.T, ctx context.Context, db *sql.DB, statement string) {
	t.Helper()
	_, err := db.ExecContext(ctx, statement)
	require.NoError(t, err, "fixture SQL %s", statement)
}

func cleanSQLServerSourceFixture(t *testing.T, db *sql.DB, schema, view string, tables, sequences []string, rangeTable string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d := dialect.NewSQLServer()
		drop := func(statement, name string) {
			if _, err := db.ExecContext(ctx, statement, name); err != nil {
				t.Errorf("clean fixture %s: %v", name, err)
			}
		}
		q := d.QualifyTable(schema, view)
		drop("IF OBJECT_ID(@p1,N'V') IS NOT NULL DROP VIEW "+q, q)
		for _, table := range tables {
			q = d.QualifyTable(schema, table)
			drop("IF OBJECT_ID(@p1,N'U') IS NOT NULL DROP TABLE "+q, q)
		}
		for _, sequence := range sequences {
			q = d.QualifyTable(schema, sequence)
			drop("IF OBJECT_ID(@p1,N'SO') IS NOT NULL EXEC("+dialect.SQLServerLiteral("DROP SEQUENCE "+q)+")", q)
		}
		if rangeTable != "" {
			function, scheme := partition.SQLServerHelperNames(schema, rangeTable)
			drop("IF EXISTS(SELECT 1 FROM sys.partition_schemes WHERE name=@p1) DROP PARTITION SCHEME "+d.QuoteIdent(scheme), scheme)
			drop("IF EXISTS(SELECT 1 FROM sys.partition_functions WHERE name=@p1) DROP PARTITION FUNCTION "+d.QuoteIdent(function), function)
		}
	})
}

func TestPostgresFamilySQLServerIntegration(t *testing.T) {
	for _, product := range []struct{ name, driver string }{
		{"postgres", "postgres"}, {"gaussdb", "opengauss"}, {"highgo", "highgo"},
		{"vastbase", "postgres"}, {"gbase", "postgres"}, {"seabox", "postgres"}, {"kingbase", "postgres"},
	} {
		t.Run(product.name, func(t *testing.T) {
			key := "DBGOLD_PG_SQLSERVER_" + strings.ToUpper(product.name) + "_DSN"
			pgDSN := os.Getenv(key)
			if pgDSN == "" {
				t.Skipf("requires dedicated source %s", key)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			targetDB, writer, targetSchema, targetMajor := openSQLServerSourceFixture(t, ctx)
			pg, err := sql.Open(product.driver, pgDSN)
			require.NoError(t, err)
			t.Cleanup(func() { _ = pg.Close() })
			var version string
			require.NoError(t, pg.QueryRowContext(ctx, "SELECT version()").Scan(&version))
			t.Logf("%s actual source version %s", product.name, version)
			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			schema, ledger, child, view := "DBGOLD_"+suffix, "Ledger_"+suffix, "LedgerChild_"+suffix, "LedgerView_"+suffix
			qSchema := source.QuotePGIdent(schema)
			sq := func(name string) string { return qSchema + "." + source.QuotePGIdent(name) }
			d := dialect.NewSQLServer()
			tq := func(name string) string { return d.QualifyTable(targetSchema, name) }
			execSQLServerSourceFixture(t, ctx, pg, "CREATE SCHEMA "+qSchema)
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				execSQLServerSourceFixture(t, cleanup, pg, "DROP SCHEMA "+qSchema+" CASCADE")
			})
			// SERIAL is available across the PG family. On newer SQL Server it
			// maps to a named default; on 2008 R2 it maps to IDENTITY.
			seq := ledger + "_Id_seq"
			shared, sharedColumn := "", ""
			if product.name == "postgres" && targetMajor >= 11 {
				shared = "SharedSeq_" + suffix
				execSQLServerSourceFixture(t, ctx, pg, "CREATE SEQUENCE "+sq(shared)+" START WITH 100 NO CYCLE CACHE 1")
				sharedColumn = `, "Ticket" bigint NOT NULL DEFAULT nextval('` + sq(shared) + `'::regclass)`
			}
			execSQLServerSourceFixture(t, ctx, pg, "CREATE TABLE "+sq(ledger)+` ("Id" bigserial PRIMARY KEY, "Name" varchar(40) NOT NULL, "Amount" numeric NOT NULL, "Payload" bytea, "Active" boolean NOT NULL DEFAULT true, "Stamp" timestamp(6) NOT NULL`+sharedColumn+")")
			execSQLServerSourceFixture(t, ctx, pg, "ALTER SEQUENCE "+sq(seq)+" RESTART WITH 20")
			execSQLServerSourceFixture(t, ctx, pg, "INSERT INTO "+sq(ledger)+` ("Name","Amount","Payload","Stamp") VALUES ('中文MiXeD',`+sqlServerSourceFixtureAmount+`,decode('00aaff','hex'),TIMESTAMP '2024-02-29 23:34:56.123456')`)
			execSQLServerSourceFixture(t, ctx, pg, "CREATE TABLE "+sq(child)+` ("Id" int PRIMARY KEY, "LedgerId" bigint NOT NULL`+sharedColumn+`, CONSTRAINT "ChildRef" FOREIGN KEY ("LedgerId") REFERENCES `+sq(ledger)+` ("Id"))`)
			execSQLServerSourceFixture(t, ctx, pg, "INSERT INTO "+sq(child)+` ("Id","LedgerId") VALUES (1,20)`)
			execSQLServerSourceFixture(t, ctx, pg, `CREATE INDEX "ChildLookup" ON `+sq(child)+` ("LedgerId" DESC)`)
			execSQLServerSourceFixture(t, ctx, pg, "COMMENT ON TABLE "+sq(ledger)+" IS 'Ledger Comment中文'")
			execSQLServerSourceFixture(t, ctx, pg, "CREATE VIEW "+sq(view)+` AS SELECT l."Name" AS "DisplayName" FROM `+sq(ledger)+` l WHERE l."Id" >= 20`)
			tables := []string{child, ledger}
			rangeTable := ""
			if product.name == "postgres" {
				rangeTable = "RangeRows_" + suffix
				execSQLServerSourceFixture(t, ctx, pg, "CREATE TABLE "+sq(rangeTable)+` ("K" bigint NOT NULL, "Label" varchar(20)) PARTITION BY RANGE ("K")`)
				for _, leaf := range []struct{ name, lo, hi string }{{"Low", "MINVALUE", "0"}, {"Mid", "0", "10"}, {"High", "10", "MAXVALUE"}} {
					execSQLServerSourceFixture(t, ctx, pg, "CREATE TABLE "+sq(rangeTable+leaf.name)+" PARTITION OF "+sq(rangeTable)+" FOR VALUES FROM ("+leaf.lo+") TO ("+leaf.hi+")")
				}
				execSQLServerSourceFixture(t, ctx, pg, "INSERT INTO "+sq(rangeTable)+" VALUES (-1,'low'),(0,'edge'),(9,'mid'),(10,'high')")
				tables = append(tables, rangeTable)
			}
			sequences := []string(nil)
			sequenceCount := 1
			if targetMajor >= 11 {
				sequences = append(sequences, seq)
				if shared != "" {
					sequences = append(sequences, shared)
					sequenceCount++
				}
			}
			cleanSQLServerSourceFixture(t, targetDB, targetSchema, view, tables, sequences, rangeTable)
			reader, err := source.NewPostgresProductForSQLServer(product.name, product.driver, pgDSN, schema, source.ConnPoolConfig{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.Close() })
			job := &Job{LogCh: make(chan string, 8192)}
			cfg := Config{Mode: "all", Content: "both", TargetDBType: "sqlserver", TargetSchema: targetSchema, PageSize: 1, MaxParallel: 2, ValidateData: true, CaptureRepair: true}
			report := NewMigrator(reader, writer, job, cfg).Run(ctx)
			require.False(t, report.HasFailures(), "migration report: %+v", report)
			require.Equal(t, len(tables), report.Tables.Success)
			require.Equal(t, 1, report.Views.Success)
			require.Equal(t, sequenceCount, report.Sequences.Success)
			require.Equal(t, 1, report.Constraints.Success)
			var label, amount, stamp string
			var payload []byte
			var active bool
			require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT [Name],CONVERT(varchar(80),[Amount]),CONVERT(varchar(40),[Stamp],126),[Payload],[Active] FROM "+tq(ledger)+" WHERE [Id]=20").Scan(&label, &amount, &stamp, &payload, &active))
			require.Equal(t, "中文MiXeD", label)
			require.Equal(t, sqlServerSourceFixtureAmount, amount)
			require.Contains(t, stamp, "2024-02-29T23:34:56.123456")
			require.Equal(t, []byte{0, 170, 255}, payload)
			require.True(t, active)
			require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT [DisplayName] FROM "+tq(view)).Scan(&label))
			require.Equal(t, "中文MiXeD", label)
			var next int64
			require.NoError(t, targetDB.QueryRowContext(ctx, "INSERT INTO "+tq(ledger)+" ([Name],[Amount],[Stamp]) OUTPUT INSERTED.[Id] VALUES(N'next',0,'2024-01-01')").Scan(&next))
			require.EqualValues(t, 21, next)
			if rangeTable != "" {
				function, _ := partition.SQLServerHelperNames(targetSchema, rangeTable)
				rows := partitionIntegrationRows(t, ctx, targetDB, "SELECT [K],$PARTITION."+d.QuoteIdent(function)+"([K]) FROM "+tq(rangeTable)+" ORDER BY [K]")
				require.Equal(t, []string{"-1|true|1", "0|true|2", "9|true|2", "10|true|3"}, rows)
			}
			if shared != "" {
				var ticket int64
				require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT [Ticket] FROM "+tq(ledger)+" WHERE [Id]=21").Scan(&ticket))
				require.EqualValues(t, 102, ticket)
				execSQLServerSourceFixture(t, ctx, pg, "DELETE FROM "+sq(child))
				execSQLServerSourceFixture(t, ctx, pg, "DELETE FROM "+sq(ledger))
				execSQLServerSourceFixture(t, ctx, pg, "INSERT INTO "+sq(ledger)+` ("Id","Name","Amount","Stamp","Ticket") VALUES (50,'appended',0,TIMESTAMP '2024-01-01',550)`)
				execSQLServerSourceFixture(t, ctx, pg, "INSERT INTO "+sq(child)+` ("Id","LedgerId","Ticket") VALUES (2,50,560)`)
				cfg.Mode, cfg.Filter, cfg.Content = "include", ledger+","+child, "data_only"
				appended := NewMigrator(reader, writer, job, cfg).Run(ctx)
				require.False(t, appended.HasFailures(), "append report: %+v; content=%+v", appended, appended.DataValidation)
				require.Equal(t, 2, appended.Data.Success)
				require.Equal(t, 2, appended.Sequences.Success)
				// The shared generator advances past the greatest imported key in
				// either table, while the owned serial advances independently.
				query := "INSERT INTO " + tq(ledger) + " ([Name],[Amount],[Stamp]) OUTPUT INSERTED.[Id],INSERTED.[Ticket] VALUES(N'next after append',0,'2024-01-01')"
				require.NoError(t, targetDB.QueryRowContext(ctx, query).Scan(&next, &ticket))
				require.EqualValues(t, 51, next)
				require.EqualValues(t, 561, ticket)
			}
		})
	}
}

func TestOracleSQLServerIntegration(t *testing.T) {
	oraDSN, owner := os.Getenv("DBGOLD_ORACLE_SQLSERVER_TEST_DSN"), os.Getenv("DBGOLD_ORACLE_SQLSERVER_TEST_OWNER")
	if oraDSN == "" || owner == "" {
		t.Skip("requires dedicated DBGOLD_ORACLE_SQLSERVER_TEST_DSN / DBGOLD_ORACLE_SQLSERVER_TEST_OWNER")
	}
	require.True(t, strings.HasPrefix(owner, "DBGOLD_"), "source owner must be dedicated and start with DBGOLD_")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	targetDB, writer, targetSchema, targetMajor := openSQLServerSourceFixture(t, ctx)
	ora, err := sql.Open("oracle", oraDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ora.Close() })
	var existing int
	var currentOwner string
	require.NoError(t, ora.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL`).Scan(&currentOwner))
	require.Equal(t, owner, currentOwner, "source test connection must belong to the dedicated fixture owner")
	require.NoError(t, ora.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=:1`, owner).Scan(&existing))
	require.Zero(t, existing, "source test owner must initially be empty")
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	ledger, child, view := "OraLedger_"+suffix, "OraChild_"+suffix, "OraView_"+suffix
	sq := func(name string) string { return source.QuotePGIdent(owner) + "." + source.QuotePGIdent(name) }
	d := dialect.NewSQLServer()
	tq := func(name string) string { return d.QualifyTable(targetSchema, name) }
	create := func(statement, cleanup string) {
		execSQLServerSourceFixture(t, ctx, ora, statement)
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			execSQLServerSourceFixture(t, cleanupCtx, ora, cleanup)
		})
	}
	create("CREATE TABLE "+sq(ledger)+` ("Id" NUMBER(18,0) PRIMARY KEY, "Name" NVARCHAR2(40) NOT NULL, "Amount" NUMBER NOT NULL, "Payload" RAW(8), "BusinessDate" DATE, "Stamp" TIMESTAMP(6))`, "DROP TABLE "+sq(ledger)+" PURGE")
	execSQLServerSourceFixture(t, ctx, ora, "INSERT INTO "+sq(ledger)+` VALUES (1,N'中文MiXeD',`+sqlServerSourceFixtureAmount+`,HEXTORAW('00aaff'),TO_DATE('2024-02-29 23:34:56','YYYY-MM-DD HH24:MI:SS'),TIMESTAMP '2024-02-29 23:34:56.123456')`)
	create("CREATE TABLE "+sq(child)+` ("Id" NUMBER(18,0) PRIMARY KEY, "LedgerId" NUMBER(18,0) NOT NULL, CONSTRAINT "ChildRef" FOREIGN KEY ("LedgerId") REFERENCES `+sq(ledger)+` ("Id"))`, "DROP TABLE "+sq(child)+" PURGE")
	execSQLServerSourceFixture(t, ctx, ora, "INSERT INTO "+sq(child)+" VALUES (2,1)")
	execSQLServerSourceFixture(t, ctx, ora, `CREATE INDEX "ChildLookup" ON `+sq(child)+` ("LedgerId")`)
	execSQLServerSourceFixture(t, ctx, ora, "COMMENT ON TABLE "+sq(ledger)+" IS 'Ledger Comment'")
	create("CREATE VIEW "+sq(view)+` AS SELECT l."Name" AS "DisplayName" FROM `+sq(ledger)+` l WHERE l."Id" > 0`, "DROP VIEW "+sq(view))
	var sequences []string
	if targetMajor >= 11 {
		sequence := "OraSeq_" + suffix
		create("CREATE SEQUENCE "+sq(sequence)+" START WITH 77 INCREMENT BY 3 MINVALUE 2 MAXVALUE 1000 NOCACHE", "DROP SEQUENCE "+sq(sequence))
		sequences = append(sequences, sequence)
	}
	cleanSQLServerSourceFixture(t, targetDB, targetSchema, view, []string{child, ledger}, sequences, "")
	reader, err := source.NewOracleForSQLServer(oraDSN, owner, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	job := &Job{LogCh: make(chan string, 8192)}
	cfg := Config{Mode: "all", Content: "both", TargetDBType: "sqlserver", TargetSchema: targetSchema, PageSize: 1, MaxParallel: 2, ValidateData: true, CaptureRepair: true}
	report := NewMigrator(reader, writer, job, cfg).Run(ctx)
	require.False(t, report.HasFailures(), "migration report: %+v", report)
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 1, report.Views.Success)
	require.Equal(t, 1, report.Constraints.Success)
	require.Equal(t, len(sequences), report.Sequences.Success)
	var label, amount, date, stamp string
	var payload []byte
	require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT [Name],CONVERT(varchar(80),[Amount]),CONVERT(varchar(40),[BusinessDate],126),CONVERT(varchar(40),[Stamp],126),[Payload] FROM "+tq(ledger)+" WHERE [Id]=1").Scan(&label, &amount, &date, &stamp, &payload))
	require.Equal(t, "中文MiXeD", label)
	require.Equal(t, sqlServerSourceFixtureAmount, amount)
	require.Contains(t, date, "2024-02-29T23:34:56")
	require.Contains(t, stamp, "2024-02-29T23:34:56.123456")
	require.Equal(t, []byte{0, 170, 255}, payload)
	require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT [DisplayName] FROM "+tq(view)).Scan(&label))
	require.Equal(t, "中文MiXeD", label)
	if len(sequences) > 0 {
		var next string
		require.NoError(t, targetDB.QueryRowContext(ctx, "SELECT CONVERT(varchar(40),NEXT VALUE FOR "+tq(sequences[0])+")").Scan(&next))
		require.Equal(t, "77", next)
	}
}
