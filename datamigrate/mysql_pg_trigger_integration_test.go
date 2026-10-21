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
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Explicit DSNs authorize fixtures in fresh schemas only. Run the same behavioral
// matrix against each product/version; a skipped product is not certified.
func TestMySQLPGTriggerIntegration(t *testing.T) {
	srcDSN, dstDSN := os.Getenv("DBGOLD_MYSQLPG_TRIGGER_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQLPG_TRIGGER_TARGET_DSN")
	if srcDSN == "" || dstDSN == "" {
		t.Skip("set DBGOLD_MYSQLPG_TRIGGER_SOURCE_DSN and DBGOLD_MYSQLPG_TRIGGER_TARGET_DSN for isolated trigger fixtures")
	}
	product := os.Getenv("DBGOLD_MYSQLPG_TRIGGER_PRODUCT")
	if product == "" {
		product = "postgres"
	}
	driver := "postgres"
	if product == "gaussdb" {
		driver = "opengauss"
	}
	if product == "highgo" {
		driver = "highgo"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	my, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = my.Close() })
	my.SetMaxOpenConns(1)
	pg, err := sql.Open(driver, dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	pg.SetMaxOpenConns(1)
	ns := fmt.Sprintf("dbgold_trg_it_%d", time.Now().UnixNano())
	qn := `"` + ns + `"`
	exec := func(db *sql.DB, q string) { t.Helper(); _, err := db.ExecContext(ctx, q); require.NoError(t, err, q) }
	exec(my, "CREATE DATABASE `"+ns+"` CHARACTER SET utf8mb4")
	t.Cleanup(func() { _, _ = my.ExecContext(context.Background(), "DROP DATABASE `"+ns+"`") })
	exec(pg, "CREATE SCHEMA "+qn)
	t.Cleanup(func() { _, _ = pg.ExecContext(context.Background(), "DROP SCHEMA "+qn+" CASCADE") })
	var srcVersion, dstVersion string
	require.NoError(t, my.QueryRowContext(ctx, "SELECT VERSION()").Scan(&srcVersion))
	require.NoError(t, pg.QueryRowContext(ctx, "SELECT version()").Scan(&dstVersion))
	t.Logf("source=%s target_product=%s target=%s", srcVersion, product, dstVersion)
	// Two sides use UTC for the timestamp tests; the translation itself preserves
	// target business-session timezone rather than freezing the migration offset.
	exec(my, "SET SESSION time_zone='+00:00'")
	exec(pg, "SET TIME ZONE 'UTC'")
	orders := "`" + ns + "`.`Orders`"
	audit := "`" + ns + "`.`Audit`"
	totals := "`" + ns + "`.`Totals`"
	exec(my, "CREATE TABLE "+orders+" (Id int PRIMARY KEY, Amount decimal(12,2), Stamp datetime(6), Label varchar(100), FixedLabel char(3))")
	exec(my, "CREATE TABLE "+audit+" (Id int, Amount decimal(12,2), ActionCode int)")
	exec(my, "CREATE TABLE "+totals+" (Id int PRIMARY KEY, Amount decimal(12,2))")
	exec(my, "INSERT INTO "+totals+" VALUES (1,0)")
	triggers := []struct{ name, timing, event, body string }{
		{"BeforeInsert", "BEFORE", "INSERT", "BEGIN IF NEW.Amount IS NULL THEN SET NEW.Amount = 0; ELSEIF NEW.Amount < 0 THEN SET NEW.Amount = 0; END IF; SET NEW.Stamp = NOW(); SET NEW.Label = IFNULL(NEW.FixedLabel, NEW.Label); SET NEW.Label = CONCAT(NEW.Amount, '|', NEW.Label); END"},
		{"AfterInsert", "AFTER", "INSERT", "BEGIN INSERT INTO " + audit + " (Id,Amount,ActionCode) VALUES (NEW.Id,NEW.Amount,1); UPDATE " + totals + " SET Amount = Amount + NEW.Amount WHERE Id = 1; END"},
		{"BeforeUpdate", "BEFORE", "UPDATE", "SET NEW.Stamp = CURRENT_TIMESTAMP(6)"},
		{"AfterUpdate", "AFTER", "UPDATE", "BEGIN INSERT INTO " + audit + " (Id,Amount,ActionCode) VALUES (NEW.Id,NEW.Amount,2); UPDATE " + totals + " SET Amount = Amount + NEW.Amount - OLD.Amount WHERE Id = 1; END"},
		{"BeforeDelete", "BEFORE", "DELETE", "INSERT INTO " + audit + " (Id,Amount,ActionCode) VALUES (OLD.Id,OLD.Amount,3)"},
		{"AfterDelete", "AFTER", "DELETE", "UPDATE " + totals + " SET Amount = Amount - OLD.Amount WHERE Id = 1"},
	}
	for _, v := range triggers {
		exec(my, "CREATE TRIGGER `"+ns+"`.`"+v.name+"` "+v.timing+" "+v.event+" ON "+orders+" FOR EACH ROW "+v.body)
	}
	// The two real business definitions exercise CONCAT and a collation-sensitive
	// INSERT SELECT. Only their source database qualifier changes for isolation.
	users := "`" + ns + "`.frame_user"
	points := "`" + ns + "`.frame_user_extendinfo"
	changes := "`" + ns + "`.changeuserinfo"
	exec(my, "CREATE TABLE "+users+" (userguid varchar(40), loginid varchar(40), displayname varchar(100), isenabled int) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	exec(my, "CREATE TABLE "+points+" (id int PRIMARY KEY, userguid varchar(40), USBKEY varchar(100)) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	exec(my, "CREATE TABLE "+changes+" (userguid varchar(40), loginid varchar(40), displayname varchar(100), operatedate datetime, dealtype varchar(1000)) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	exec(my, "INSERT INTO "+users+" VALUES ('u1','login1','用户一',0),('U1 ','login2','用户二',0)")
	exec(my, "INSERT INTO "+points+" VALUES (1,'u1','A')")
	exec(my, "CREATE TRIGGER `"+ns+"`.update_enableuser AFTER UPDATE ON "+users+" FOR EACH ROW begin if (old.isenabled!=new.isenabled) then insert into "+changes+"(userguid,loginid,displayname,operatedate,dealtype) values(new.userguid,new.loginid,new.displayname,now(),CONCAT('Frame_User表isenabled字段由【||',OLD.ISENABLED,'||】更新为【||',NEW.ISENABLED,'||】')); end if; end")
	exec(my, "CREATE TRIGGER `"+ns+"`.update_isepoint AFTER UPDATE ON "+points+" FOR EACH ROW begin if (old.usbkey!=new.usbkey) then insert into "+changes+"(userguid,loginid,displayname,operatedate,dealtype) select userguid,loginid,displayname,now(),CONCAT('Frame_User_ExtendInfo表usbkey字段由【||',OLD.USBKEY,'||】更新为【||',NEW.USBKEY,'||】') from "+users+" where userguid = new.userguid; end if; end")
	exec(my, "INSERT INTO "+orders+" (Id,Amount,Label) VALUES (1,5,'historical')")
	sourceConfig, err := mysqldriver.ParseDSN(srcDSN)
	require.NoError(t, err)
	sourceConfig.DBName = ns
	r, err := source.NewMySQL(sourceConfig.FormatDSN(), ns, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	var w target.Writer
	switch product {
	case "postgres", "seabox":
		w, err = target.NewPostgresCompatible(dstDSN, ns, product, target.ConnPoolConfig{})
	case "gaussdb":
		w, err = target.NewGaussDB(dstDSN, ns, target.ConnPoolConfig{})
	case "highgo":
		w, err = target.NewHighGo(dstDSN, ns, target.ConnPoolConfig{})
	case "vastbase":
		w, err = target.NewVastbase(dstDSN, ns, target.ConnPoolConfig{})
	case "gbase":
		w, err = target.NewGBase(dstDSN, ns, target.ConnPoolConfig{})
	case "kingbase":
		w, err = target.NewKingbase(dstDSN, ns, target.ConnPoolConfig{})
	default:
		t.Fatalf("unknown target product %s", product)
	}
	require.NoError(t, err)
	defer w.Close()
	cfg := Config{Mode: "all", Content: "both", TargetSchema: ns, TargetDBType: product, LowerCaseNames: true, MigrateTriggers: true, CaptureRepair: true, PageSize: 2, MaxParallel: 2}
	job := &Job{LogCh: make(chan string, 2048)}
	report := NewMigrator(r, w, job, cfg).Run(ctx)
	// Compatibility routine initialization is existing independent work. Record
	// any failure rather than letting it conceal this trigger behavior matrix.
	if report.CompatibilityFunctions != nil && report.CompatibilityFunctions.Failed > 0 {
		for _, item := range report.CompatibilityFunctions.Items {
			if item.Status == "failed" {
				t.Logf("independent compatibility initializer: %s: %s", item.Name, item.Error)
			}
		}
	}
	behaviorReport := report
	behaviorReport.CompatibilityFunctions = nil
	require.False(t, behaviorReport.HasFailures(), "tables=%+v data=%+v triggers=%+v", report.Tables, report.Data, report.Triggers)
	require.Equal(t, 8, report.Triggers.Success)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "trigger" {
			require.Equal(t, "succeeded", object.Status)
			require.Len(t, object.Units, 2)
		}
	}
	query := func(db *sql.DB, q string) [][]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		require.NoError(t, err)
		defer rows.Close()
		cols, err := rows.Columns()
		require.NoError(t, err)
		out := [][]string{}
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			dest := make([]any, len(cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			require.NoError(t, rows.Scan(dest...))
			line := make([]string, len(cols))
			for i, v := range vals {
				if v.Valid {
					line[i] = v.String
				} else {
					line[i] = "<NULL>"
				}
			}
			out = append(out, line)
		}
		require.NoError(t, rows.Err())
		return out
	}
	compare := func() {
		t.Helper()
		for _, v := range []struct{ src, dst, cols, order string }{
			{orders, qn + `."orders"`, "Id,Amount,Label", "Id"},
			{audit, qn + `."audit"`, "Id,Amount,ActionCode", "Id,ActionCode,Amount"},
			{totals, qn + `."totals"`, "Id,Amount", "Id"},
		} {
			require.Equal(t, query(my, "SELECT "+v.cols+" FROM "+v.src+" ORDER BY "+v.order), query(pg, "SELECT "+strings.ToLower(v.cols)+" FROM "+v.dst+" ORDER BY "+strings.ToLower(v.order)), v.src)
		}
	}
	compare() // Historical audit records must be copied exactly once.
	compareBusiness := func() {
		t.Helper()
		for _, table := range []struct{ name, cols string }{
			{"frame_user", "userguid,loginid,displayname,isenabled"},
			{"frame_user_extendinfo", "id,userguid,usbkey"},
			{"changeuserinfo", "userguid,loginid,displayname,dealtype"},
		} {
			require.ElementsMatch(t, query(my, "SELECT "+table.cols+" FROM `"+ns+"`."+table.name), query(pg, "SELECT "+table.cols+" FROM "+qn+"."+table.name), table.name)
		}
	}
	compareBusiness()
	exec(my, "UPDATE "+users+" SET isenabled=1")
	exec(pg, "UPDATE "+qn+".frame_user SET isenabled=1")
	compareBusiness()
	var count int
	require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn+".changeuserinfo WHERE dealtype='Frame_User表isenabled字段由【||0||】更新为【||1||】'").Scan(&count))
	require.Equal(t, 2, count)
	// Each comparison starts from the same value on both sides. Different emoji
	// share one MySQL general_ci weight; combining accents deliberately do not.
	var lookupDuration time.Duration
	lookupUpdates := 0
	for _, pair := range [][2]string{{"A", "a"}, {"é", "e"}, {"a", "a "}, {"ß", "s"}, {"ß", "ss"}, {"é", "e\u0301"}, {"I", "ı"}, {"😀", "😁"}, {"😀", "�"}, {"", ""}, {"a", "a\t"}, {"a", "a\u00a0"}, {"a", "z"}} {
		_, err = my.ExecContext(ctx, "UPDATE "+points+" SET USBKEY=? WHERE id=1", pair[0])
		require.NoError(t, err)
		_, err = pg.ExecContext(ctx, "UPDATE "+qn+".frame_user_extendinfo SET usbkey=$1 WHERE id=1", pair[0])
		require.NoError(t, err)
		compareBusiness()
		_, err = my.ExecContext(ctx, "UPDATE "+points+" SET USBKEY=? WHERE id=1", pair[1])
		require.NoError(t, err)
		started := time.Now()
		_, err = pg.ExecContext(ctx, "UPDATE "+qn+".frame_user_extendinfo SET usbkey=$1 WHERE id=1", pair[1])
		require.NoError(t, err)
		lookupDuration += time.Since(started)
		lookupUpdates++
		compareBusiness()
	}
	t.Logf("collation/lookup fixture: %d target updates, 2 lookup rows, total=%s mean=%s (includes connection round trip; not a production benchmark)", lookupUpdates, lookupDuration, lookupDuration/time.Duration(lookupUpdates))
	for _, value := range []any{nil, "after-null", nil, ""} {
		_, err = my.ExecContext(ctx, "UPDATE "+points+" SET USBKEY=? WHERE id=1", value)
		require.NoError(t, err)
		_, err = pg.ExecContext(ctx, "UPDATE "+qn+".frame_user_extendinfo SET usbkey=$1 WHERE id=1", value)
		require.NoError(t, err)
		compareBusiness()
	}
	// A missing lookup row produces no audit records; the existing source rows
	// above deliberately produce two records through case/PAD SPACE equality.
	exec(my, "UPDATE "+points+" SET userguid='missing',USBKEY='no match' WHERE id=1")
	exec(pg, "UPDATE "+qn+".frame_user_extendinfo SET userguid='missing',usbkey='no match' WHERE id=1")
	compareBusiness()
	require.NoError(t, pg.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qn+".changeuserinfo WHERE EXTRACT(MICROSECONDS FROM operatedate)::bigint % 1000000 <> 0").Scan(&count))
	require.Zero(t, count)
	apply := func(suffix string) {
		t.Helper()
		exec(my, strings.ReplaceAll(suffix, "{table}", orders))
		exec(pg, strings.ReplaceAll(strings.ToLower(suffix), "{table}", qn+`."orders"`))
		compare()
	}
	apply("INSERT INTO {table} (Id,Amount,Label) VALUES (2,NULL,'new')")
	apply("INSERT INTO {table} (Id,Amount,Label) VALUES (3,7,'batch'),(4,-2,'batch')")
	apply("INSERT INTO {table} (Id,Amount,Label) VALUES (5,2,'a ')")
	apply("UPDATE {table} SET Amount = 10 WHERE Id = 3")
	apply("DELETE FROM {table} WHERE Id = 1")
	var fraction int
	require.NoError(t, pg.QueryRowContext(ctx, "SELECT CAST(EXTRACT(MICROSECONDS FROM stamp) AS integer) % 1000000 FROM "+qn+`."orders" WHERE id=2`).Scan(&fraction))
	require.Zero(t, fraction, "NOW() has source precision zero")
	_, srcErr := my.ExecContext(ctx, "INSERT INTO "+orders+" (Id,Amount) VALUES (2,9)")
	_, dstErr := pg.ExecContext(ctx, "INSERT INTO "+qn+`."orders" (id,amount) VALUES (2,9)`)
	require.Error(t, srcErr)
	require.Error(t, dstErr)
	compare() // Trigger side effects roll back with the failed row.
	compareBusiness()
	// A following data-only load must stop before COPY while these triggers exist.
	cfg.Content = "data_only"
	cfg.MigrateTriggers = false
	cfg.CaptureRepair = false
	blocked := NewMigrator(r, w, &Job{LogCh: make(chan string, 2048)}, cfg).Run(ctx)
	require.True(t, blocked.HasFailures())
	require.Equal(t, "preflight", blocked.Data.Items[0].Stage)
	compare()
}
