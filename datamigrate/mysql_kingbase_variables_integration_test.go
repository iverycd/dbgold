package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Opt-in credentials are used only for fresh, randomly named fixture namespaces.
// Pin business connections because native user variables belong to a connection.
func TestMySQLKingbaseNativeVariableIntegration(t *testing.T) {
	srcDSN := os.Getenv("DBGOLD_MYSQLPG_TRIGGER_SOURCE_DSN")
	dstDSN := os.Getenv("DBGOLD_MYSQLPG_TRIGGER_TARGET_DSN")
	if srcDSN == "" || dstDSN == "" || os.Getenv("DBGOLD_MYSQLPG_TRIGGER_PRODUCT") != "kingbase" {
		t.Skip("set MySQL/Kingbase trigger DSNs and DBGOLD_MYSQLPG_TRIGGER_PRODUCT=kingbase for isolated native-variable fixtures")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	my, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, my.Close()) })
	pg, err := sql.Open("postgres", dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	ns := fmt.Sprintf("dbgold_native_it_%d", time.Now().UnixNano())
	qs := func(name string) string { return "`" + ns + "`.`" + name + "`" }
	qt := func(name string) string { return `"` + ns + `"."` + name + `"` }
	_, err = my.ExecContext(ctx, "CREATE DATABASE `"+ns+"` CHARACTER SET utf8mb4")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := my.ExecContext(context.Background(), "DROP DATABASE `"+ns+"`")
		require.NoError(t, err)
	})
	_, err = pg.ExecContext(ctx, `CREATE SCHEMA "`+ns+`"`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pg.ExecContext(context.Background(), `DROP SCHEMA "`+ns+`" CASCADE`)
		require.NoError(t, err)
	})
	for _, statement := range []string{
		"CREATE TABLE " + qs("model_maxnumber") + " (Id int PRIMARY KEY, CurrentValue int)",
		"CREATE TABLE " + qs("native_bigint") + " (Id int PRIMARY KEY, CurrentValue bigint)",
		"INSERT INTO " + qs("model_maxnumber") + " VALUES (1,2147483647),(2,NULL),(3,-2147483648)",
		"INSERT INTO " + qs("native_bigint") + " VALUES (1,9223372036854775807),(2,-9223372036854775808)",
		"CREATE TRIGGER `" + ns + "`.modelmaxnumber_before BEFORE UPDATE ON " + qs("model_maxnumber") + " FOR EACH ROW SET @CurrentValue=OLD.CurrentValue+1",
		"CREATE TRIGGER `" + ns + "`.modelmaxnumber_after AFTER INSERT ON " + qs("model_maxnumber") + " FOR EACH ROW SET @CurrentValue=NEW.CurrentValue",
		"CREATE TRIGGER `" + ns + "`.native_big_before BEFORE UPDATE ON " + qs("native_bigint") + " FOR EACH ROW SET @BigValue=OLD.CurrentValue+1",
	} {
		_, err = my.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	sourceConfig, err := mysqldriver.ParseDSN(srcDSN)
	require.NoError(t, err)
	sourceConfig.DBName = ns
	r, err := source.NewMySQL(sourceConfig.FormatDSN(), ns, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	w, err := target.NewKingbase(dstDSN, ns, target.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	supported, err := target.DetectMySQLPGNativeUserVariables(ctx, w)
	require.NoError(t, err)
	require.True(t, supported, "fixture requires native user variables in Kingbase mysql mode")
	var srcVersion, srcComment, dstVersion string
	require.NoError(t, my.QueryRowContext(ctx, "SELECT VERSION(),@@version_comment").Scan(&srcVersion, &srcComment))
	require.NoError(t, pg.QueryRowContext(ctx, "SELECT version()").Scan(&dstVersion))
	t.Logf("source=%s %s target=%s", srcVersion, srcComment, dstVersion)
	cfg := Config{Mode: "all", Content: "both", TargetSchema: ns, TargetDBType: "kingbase", LowerCaseNames: true,
		MigrateTriggers: true, CaptureRepair: true, PageSize: 2, MaxParallel: 1}
	job := &Job{LogCh: make(chan string, 2048)}
	report := NewMigrator(r, w, job, cfg).Run(ctx)
	// Compatibility-function initialization has its own report category.
	require.Zero(t, report.Tables.Failed, "%+v", report.Tables.Items)
	require.Zero(t, report.Data.Failed, "%+v", report.Data.Items)
	require.Equal(t, 3, report.Triggers.Success, "%+v", report.Triggers.Items)
	require.Zero(t, report.Triggers.Failed, "%+v", report.Triggers.Items)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind != "trigger" {
			continue
		}
		require.True(t, object.MySQLPGTrigger.NativeUserVariables)
		require.Equal(t, mysqltrigger.CurrentVersion, object.MySQLPGTrigger.Version)
		require.NoError(t, mysqltrigger.ValidatePlan(*object.MySQLPGTrigger))
	}
	mc, err := my.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mc.Close()) })
	pc, err := pg.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pc.Close()) })
	variable := func(conn *sql.Conn, name string) sql.NullString {
		t.Helper()
		var value sql.NullString
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @"+name).Scan(&value))
		return value
	}
	checkVariable := func(name string, want sql.NullString) {
		t.Helper()
		require.Equal(t, want, variable(mc, name), "source %s", name)
		require.Equal(t, want, variable(pc, name), "target %s", name)
	}
	execPair := func(src, dst string) {
		t.Helper()
		_, err := mc.ExecContext(ctx, src)
		require.NoError(t, err, src)
		_, err = pc.ExecContext(ctx, dst)
		require.NoError(t, err, dst)
	}
	rowPair := func(id int, want sql.NullInt64) {
		t.Helper()
		var a, b sql.NullInt64
		require.NoError(t, mc.QueryRowContext(ctx, "SELECT CurrentValue FROM "+qs("model_maxnumber")+" WHERE Id=?", id).Scan(&a))
		require.NoError(t, pc.QueryRowContext(ctx, "SELECT currentvalue FROM "+qt("model_maxnumber")+" WHERE id=$1", id).Scan(&b))
		require.Equal(t, want, a)
		require.Equal(t, want, b)
	}
	execPair("INSERT INTO "+qs("model_maxnumber")+" VALUES (4,7)", "INSERT INTO "+qt("model_maxnumber")+" VALUES (4,7)")
	checkVariable("currentvalue", sql.NullString{String: "7", Valid: true})
	rowPair(4, sql.NullInt64{Int64: 7, Valid: true})
	execPair("INSERT INTO "+qs("model_maxnumber")+" VALUES (5,NULL)", "INSERT INTO "+qt("model_maxnumber")+" VALUES (5,NULL)")
	checkVariable("CurrentValue", sql.NullString{})
	execPair("UPDATE "+qs("model_maxnumber")+" SET CurrentValue=20 WHERE Id=1", "UPDATE "+qt("model_maxnumber")+" SET currentvalue=20 WHERE id=1")
	checkVariable("CURRENTVALUE", sql.NullString{String: "2147483648", Valid: true})
	rowPair(1, sql.NullInt64{Int64: 20, Valid: true})
	execPair("UPDATE "+qs("model_maxnumber")+" SET CurrentValue=-9 WHERE Id=3", "UPDATE "+qt("model_maxnumber")+" SET currentvalue=-9 WHERE id=3")
	checkVariable("CurrentValue", sql.NullString{String: "-2147483647", Valid: true})
	execPair("UPDATE "+qs("model_maxnumber")+" SET CurrentValue=1 WHERE Id=2", "UPDATE "+qt("model_maxnumber")+" SET currentvalue=1 WHERE id=2")
	checkVariable("CurrentValue", sql.NullString{})
	rowPair(2, sql.NullInt64{Int64: 1, Valid: true})
	// A second physical connection cannot see the first connection's variables.
	for _, db := range []*sql.DB{my, pg} {
		other, err := db.Conn(ctx)
		require.NoError(t, err)
		require.False(t, variable(other, "CurrentValue").Valid)
		require.NoError(t, other.Close())
	}
	for _, conn := range []*sql.Conn{mc, pc} {
		tx, err := conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		table := qt("model_maxnumber")
		if conn == mc {
			table = qs("model_maxnumber")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" VALUES (6,11)")
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		var rows int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE id=6").Scan(&rows))
		require.Zero(t, rows)
	}
	checkVariable("CurrentValue", sql.NullString{String: "11", Valid: true})
	for _, conn := range []*sql.Conn{mc, pc} {
		tx, err := conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		table := qt("model_maxnumber")
		if conn == mc {
			table = qs("model_maxnumber")
		}
		_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET currentvalue=99 WHERE id=1")
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
	}
	checkVariable("CurrentValue", sql.NullString{String: "21", Valid: true})
	rowPair(1, sql.NullInt64{Int64: 20, Valid: true})
	// Both systems reject signed bigint overflow before assigning the variable.
	_, srcOverflow := mc.ExecContext(ctx, "UPDATE "+qs("native_bigint")+" SET CurrentValue=0 WHERE Id=1")
	_, dstOverflow := pc.ExecContext(ctx, "UPDATE "+qt("native_bigint")+" SET currentvalue=0 WHERE id=1")
	require.Error(t, srcOverflow)
	require.Error(t, dstOverflow)
	var mysqlOverflow *mysqldriver.MySQLError
	require.ErrorAs(t, srcOverflow, &mysqlOverflow)
	require.EqualValues(t, 1690, mysqlOverflow.Number)
	var kingbaseOverflow *pq.Error
	require.ErrorAs(t, dstOverflow, &kingbaseOverflow)
	require.Equal(t, pq.ErrorCode("22003"), kingbaseOverflow.Code)
	checkVariable("BigValue", sql.NullString{})
	execPair("UPDATE "+qs("native_bigint")+" SET CurrentValue=0 WHERE Id=2", "UPDATE "+qt("native_bigint")+" SET currentvalue=0 WHERE id=2")
	checkVariable("BigValue", sql.NullString{String: "-9223372036854775807", Valid: true})
	// Replanning an old unsupported object uses the same source DDL and can reuse
	// an exact installed plan through the existing two-unit repair transaction.
	var original repair.Object
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "trigger" && object.SourceName == "modelmaxnumber_before" {
			original = object
			break
		}
	}
	require.NotEmpty(t, original.ID)
	original.MySQLPGTrigger, original.Units = nil, nil
	original.Status, original.OriginalStatus = "failed", "failed"
	original.Error, original.OriginalError = "仅支持 SET NEW.列 = 表达式", "仅支持 SET NEW.列 = 表达式"
	original.DiagnosticOnly = true
	candidate, err := BuildMySQLPGTriggerRepairPlan(ctx, r, w, cfg, original, report.RepairManifest.Objects)
	require.NoError(t, err)
	require.True(t, candidate.MySQLPGTrigger.NativeUserVariables)
	require.Equal(t, original.OriginalError, candidate.OriginalError)
	_, err = target.ExecuteRepair(ctx, w, candidate, candidate.Units, nil)
	require.NoError(t, err)
	// MySQL can promote negation of MIN_INT64 beyond the signed range. Such
	// an operand cannot be translated as bigint negation without changing it.
	var negatedMinimum string
	require.NoError(t, mc.QueryRowContext(ctx, "SELECT -CAST(-9223372036854775808 AS SIGNED)").Scan(&negatedMinimum))
	require.Equal(t, "9223372036854775808", negatedMinimum)
	bigTable, err := r.GetTableDDLInfo(ctx, "native_bigint")
	require.NoError(t, err)
	characters, err := r.GetTriggerCharacterMetadata(ctx)
	require.NoError(t, err)
	unsafe, err := mysqltrigger.Convert(source.TriggerInfo{Schema: ns, TableName: "native_bigint", Name: "unsafe_negation",
		Timing: "BEFORE", Event: "UPDATE", Body: "SET @BigValue=-OLD.CurrentValue"},
		map[string]*source.TableDDLInfo{"native_bigint": bigTable},
		mysqltrigger.Options{Schema: ns, CharacterMetadata: characters, NativeUserVariables: true})
	require.ErrorContains(t, err, "MIN_INT64")
	require.Empty(t, unsafe.FunctionSQL)
	require.Empty(t, unsafe.TriggerSQL)
}
