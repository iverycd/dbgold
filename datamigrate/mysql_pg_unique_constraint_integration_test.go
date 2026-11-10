package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// The opt-in connections must be fixture servers. The test owns its source
// database and target schema, which are created and dropped for this run.
func TestMySQLPGUniqueConstraintIntegration(t *testing.T) {
	sourceDSN := os.Getenv("DBGOLD_MYSQLPG_UNIQUE_SOURCE_DSN")
	targetDSN := os.Getenv("DBGOLD_MYSQLPG_UNIQUE_TARGET_DSN")
	if sourceDSN == "" || targetDSN == "" {
		t.Skip("set DBGOLD_MYSQLPG_UNIQUE_SOURCE_DSN and DBGOLD_MYSQLPG_UNIQUE_TARGET_DSN for disposable MySQL 8.0 and PostgreSQL servers")
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

	namespace := fmt.Sprintf("dbgold_pg_unique_%x", time.Now().UnixNano())
	pgSchema := `"` + namespace + `"`
	mySchema := "`" + namespace + "`"
	exec := func(t *testing.T, db *sql.DB, sql string) {
		t.Helper()
		_, err := db.ExecContext(ctx, sql)
		require.NoError(t, err, "%s", sql)
	}
	exec(t, my, "CREATE DATABASE "+mySchema+" CHARACTER SET utf8mb4")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := my.ExecContext(cleanup, "DROP DATABASE "+mySchema)
		require.NoError(t, err)
	})
	exec(t, pg, "CREATE SCHEMA "+pgSchema)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := pg.ExecContext(cleanup, "DROP SCHEMA "+pgSchema+" CASCADE")
		require.NoError(t, err)
	})
	srcConfig, err := mysql.ParseDSN(sourceDSN)
	require.NoError(t, err)
	srcConfig.DBName = namespace
	r, err := source.NewMySQL(srcConfig.FormatDSN(), namespace, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	w, err := target.NewPostgres(targetDSN, namespace, target.ConnPoolConfig{})
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
	repairIndex := func(t *testing.T, report MigrationReport, name string) repair.Object {
		t.Helper()
		require.NotNil(t, report.RepairManifest)
		for _, obj := range report.RepairManifest.Objects {
			if obj.Kind == "index" && obj.SourceName == name {
				return obj
			}
		}
		t.Fatalf("repair manifest missing index %s", name)
		return repair.Object{}
	}
	const constraintDefinitionSQL = `SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid
JOIN pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=$1 AND t.relname=$2 AND c.conname=$3 AND c.contype='u'`
	requireConstraint := func(t *testing.T, table, name string) {
		t.Helper()
		var definition string
		require.NoError(t, pg.QueryRowContext(ctx, constraintDefinitionSQL, namespace, table, name).Scan(&definition))
		require.Equal(t, "UNIQUE (channel, outcasecode)", definition)
	}

	t.Run("screenshot and index-only retry", func(t *testing.T) {
		const table, constraint = "onm_event_case", "uk_channel_outcase"
		src := mySchema + ".`" + table + "`"
		dst := pgSchema + `."` + table + `"`
		exec(t, my, "CREATE TABLE "+src+" (id INT PRIMARY KEY, channel VARCHAR(24), outcasecode VARCHAR(24), "+
			"UNIQUE KEY `"+constraint+"` (((case when (`outcasecode` <> _utf8mb4'') then concat(`channel`,_utf8mb4':',`outcasecode`) end))))")
		exec(t, my, "INSERT INTO "+src+" VALUES (1,'sms','abc'),(2,'sms',''),(3,'a:b','c'),"+
			"(4,'sms',NULL),(5,'sms',NULL),(6,NULL,'abc'),(7,NULL,'abc')")
		report := run(table, false, false)
		require.False(t, report.HasFailures(), "%+v", report)
		require.Equal(t, 1, report.Indexes.Success)
		requireConstraint(t, table, constraint)
		obj := repairIndex(t, report, constraint)
		require.NotNil(t, obj.Index)
		require.True(t, obj.Index.PGUniqueConstraint)
		require.Contains(t, obj.DDL, `ADD CONSTRAINT "uk_channel_outcase" UNIQUE ("channel", "outcasecode")`)
		require.Len(t, report.Warnings, 1)
		require.Contains(t, report.Warnings[0].Reason, "普通联合唯一约束")
		verified, err := target.InspectRepair(ctx, w, obj)
		require.NoError(t, err)
		require.Equal(t, "succeeded", verified.Status, "%+v", verified)

		exec(t, pg, "INSERT INTO "+dst+" VALUES (8,'a','b:c')")
		for _, values := range []string{"(9,'sms','abc')", "(10,'sms','')"} {
			_, err = pg.ExecContext(ctx, "INSERT INTO "+dst+" VALUES "+values)
			var coded interface{ SQLState() string }
			require.ErrorAs(t, err, &coded)
			require.Equal(t, "23505", coded.SQLState())
		}
		report = run(table, true, false)
		require.Zero(t, report.Indexes.Failed, "%+v", report.Indexes)
		require.Equal(t, 1, report.Indexes.Success)
		require.Zero(t, report.Data.Total)
		require.Zero(t, report.Tables.Success)
		var count int
		require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dst).Scan(&count))
		require.Equal(t, 8, count)

		exec(t, pg, "ALTER TABLE "+dst+` DROP CONSTRAINT "`+constraint+`"`)
		exec(t, pg, `CREATE UNIQUE INDEX "`+constraint+`" ON `+dst+` (channel, outcasecode)`)
		report = run(table, true, false)
		require.Zero(t, report.Indexes.Success)
		require.Equal(t, 1, report.Indexes.Failed, "%+v", report.Indexes)
		var definition string
		require.ErrorIs(t, pg.QueryRowContext(ctx, constraintDefinitionSQL, namespace, table, constraint).Scan(&definition), sql.ErrNoRows)
		exec(t, pg, "DROP INDEX "+pgSchema+`."`+constraint+`"`)
		report = run(table, true, false)
		require.Equal(t, 1, report.Indexes.Success, "%+v", report.Indexes)
		requireConstraint(t, table, constraint)
	})

	t.Run("duplicate data and repair", func(t *testing.T) {
		const table, mappedTable, constraint = "DupCase", "dupcase", "aa_duplicate"
		src := mySchema + ".`" + table + "`"
		dst := pgSchema + `."` + mappedTable + `"`
		exec(t, my, "CREATE TABLE "+src+" (Id INT PRIMARY KEY, Channel VARCHAR(24), OutCaseCode VARCHAR(24), Payload VARCHAR(24), "+
			"UNIQUE KEY "+constraint+" ((CASE WHEN `OutCaseCode` <> '' THEN CONCAT(`Channel`,':',`OutCaseCode`) ELSE NULL END) DESC), "+
			"UNIQUE KEY bb_unsupported ((lower(Payload))), KEY zz_payload (Payload))")
		exec(t, my, "INSERT INTO "+src+" VALUES (1,'sms','','one'),(2,'sms','','two')")
		report := run(table, false, true)
		require.Equal(t, 1, report.Data.Success, "%+v", report.Data)
		require.Equal(t, 2, report.Indexes.Failed, "%+v", report.Indexes)
		require.Equal(t, 1, report.Indexes.Success, "%+v", report.Indexes)
		require.Len(t, report.Indexes.Items, 2)
		duplicate := report.Indexes.Items[0]
		require.Equal(t, constraint, duplicate.SourceName)
		require.Equal(t, "execute", duplicate.Stage)
		require.Contains(t, duplicate.DDL, dst+` ADD CONSTRAINT "`+constraint+`" UNIQUE ("channel", "outcasecode")`)
		require.NotEmpty(t, duplicate.Error)
		require.NotEmpty(t, duplicate.Suggestion)
		unsupported := report.Indexes.Items[1]
		require.Equal(t, "bb_unsupported", unsupported.SourceName)
		require.Equal(t, "unsupported", unsupported.Stage)
		require.Empty(t, unsupported.DDL)
		require.Contains(t, unsupported.SourceDDL, "bb_unsupported")
		var laterIndex bool
		require.NoError(t, pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname=$1 AND tablename=$2 AND indexname='zz_payload')`, namespace, mappedTable).Scan(&laterIndex))
		require.True(t, laterIndex)

		obj := repairIndex(t, report, constraint)
		require.Equal(t, duplicate.DDL, obj.DDL)
		require.Len(t, obj.Units, 1)
		exec(t, pg, "UPDATE "+dst+" SET outcasecode='new-code' WHERE id=2")
		units, err := target.ExecuteRepair(ctx, w, obj, obj.Units, nil)
		require.NoError(t, err)
		require.Equal(t, "succeeded", units[0].Status)
		requireConstraint(t, mappedTable, constraint)
		verified, err := target.InspectRepair(ctx, w, obj)
		require.NoError(t, err)
		require.Equal(t, "succeeded", verified.Status, "%+v", verified)
		units, err = target.ExecuteRepair(ctx, w, obj, obj.Units, nil)
		require.NoError(t, err, "retry can reuse the constraint when the saved unit still says failed")
		require.Equal(t, "succeeded", units[0].Status)
	})
}
