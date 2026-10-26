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
	mysql "github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Run only against disposable source and target schemas. Trigger execution is
// compared through actual subsequent business DML, not just CREATE acceptance.
func TestMySQLDamengTriggerIntegration(t *testing.T) {
	srcDSN, dstDSN, schema := os.Getenv("MYSQL_DM_TEST_SOURCE_DSN"), os.Getenv("MYSQL_DM_TEST_TARGET_DSN"), os.Getenv("MYSQL_DM_TEST_SCHEMA")
	if os.Getenv("MYSQL_DM_TRIGGER_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || schema == "" {
		t.Skip("需要独立 MySQL/达梦测试 schema、MYSQL_DM_TEST_SOURCE_DSN / TARGET_DSN / SCHEMA 以及 MYSQL_DM_TRIGGER_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := mysql.ParseDSN(srcDSN)
	require.NoError(t, err)
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	require.NoError(t, src.PingContext(ctx))
	require.NoError(t, dst.PingContext(ctx))
	prefix := fmt.Sprintf("Trg%d", time.Now().UnixNano()%1000000000)
	orders, audit, lookup := prefix+"Orders", prefix+"Audit", prefix+"Lookup"
	sq := func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
	dq := func(s string) string { return dialect.NewDaMeng().QualifyTable(schema, s) }
	execSource := func(s string) { t.Helper(); _, e := src.ExecContext(ctx, s); require.NoError(t, e, s) }
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, table := range []string{orders, audit, lookup} {
			_, _ = src.ExecContext(cleanup, "DROP TABLE IF EXISTS "+sq(table))
			_, _ = dst.ExecContext(cleanup, "DROP TABLE "+dq(table)+" CASCADE CONSTRAINTS")
		}
	})
	execSource("CREATE TABLE " + sq(orders) + " (Id INT NOT NULL PRIMARY KEY, Amount DECIMAL(12,2) NULL, Hits INT NOT NULL) ENGINE=InnoDB")
	execSource("CREATE TABLE " + sq(audit) + " (OrderId INT NOT NULL, Evt VARCHAR(8) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL, Hits INT NOT NULL, Amount DECIMAL(12,2) NULL) ENGINE=InnoDB")
	execSource("CREATE TABLE " + sq(lookup) + " (Id INT NOT NULL PRIMARY KEY, Weight INT NOT NULL) ENGINE=InnoDB")
	execSource("INSERT INTO " + sq(orders) + " VALUES (1,5,0)")
	execSource("INSERT INTO " + sq(audit) + " VALUES (0,'H',999,0)")
	execSource("INSERT INTO " + sq(lookup) + " VALUES (2,9)")
	triggers := []struct{ name, timing, event, body string }{
		{"BI1", "BEFORE", "INSERT", `BEGIN DECLARE n INT DEFAULT 0; SET NEW.Amount=IFNULL(NEW.Amount,100); WHILE n<2 DO SET NEW.Hits=NEW.Hits+1; SET n=n+1; END WHILE; END`},
		{"BI2", "BEFORE", "INSERT", `SET NEW.Hits=NEW.Hits+10`},
		{"BU", "BEFORE", "UPDATE", `BEGIN DECLARE v INT DEFAULT 7; DECLARE n INT DEFAULT 0; SELECT COUNT(*) INTO n FROM ` + sq(lookup) + ` WHERE Id=NEW.Id; SET n=0; SELECT Weight INTO v FROM ` + sq(lookup) + ` WHERE Id=NEW.Id; outer_loop: LOOP SET n=n+1; IF n=1 THEN ITERATE outer_loop; END IF; LEAVE outer_loop; END LOOP; REPEAT SET n=n+1; UNTIL n>=3 END REPEAT; CASE WHEN NEW.Amount<0 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='negative amount'; ELSE SET NEW.Hits=v+n; END CASE; END`},
		{"BD", "BEFORE", "DELETE", `BEGIN IF OLD.Amount<0 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='negative delete'; END IF; END`},
		{"AI", "AFTER", "INSERT", `INSERT INTO ` + sq(audit) + ` (OrderId,Evt,Hits,Amount) VALUES (NEW.Id,'I',NEW.Hits,NEW.Amount)`},
		{"AU", "AFTER", "UPDATE", `INSERT INTO ` + sq(audit) + ` (OrderId,Evt,Hits,Amount) VALUES (NEW.Id,'U',NEW.Hits,NEW.Amount)`},
		{"AD", "AFTER", "DELETE", `INSERT INTO ` + sq(audit) + ` (OrderId,Evt,Hits,Amount) VALUES (OLD.Id,'D',OLD.Hits,OLD.Amount)`},
	}
	for _, tr := range triggers {
		execSource("CREATE TRIGGER " + sq(prefix+tr.name) + " " + tr.timing + " " + tr.event + " ON " + sq(orders) + " FOR EACH ROW " + tr.body)
	}
	reader, err := source.NewMySQLForDameng(srcDSN, cfg.DBName, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer reader.Close()
	writer, err := target.NewDaMeng(dstDSN, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer writer.Close()
	job := &Job{LogCh: make(chan string, 4096)}
	m := NewMigrator(reader, writer, job, Config{Mode: "include", Filter: strings.Join([]string{orders, audit, lookup}, ","), Content: "both", TargetSchema: schema, TargetDBType: "dameng", PageSize: 10, MaxParallel: 1, MigrateTriggers: true, CaptureRepair: true})
	report := m.Run(ctx)
	require.Equal(t, len(triggers), report.Triggers.Total, "report=%+v", report)
	require.Equal(t, len(triggers), report.Triggers.Success, "failures=%+v logs=%v", report.Triggers.Items, drainLogs(job))
	require.False(t, report.HasFailures(), "report=%+v", report)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(audit)).Scan(&count))
	require.Equal(t, 1, count, "historical load must not fire audit triggers")
	// Reconcile an identical target through the normal immutable installation
	// path; this must reuse rather than overwrite or duplicate the trigger.
	for _, o := range report.RepairManifest.Objects {
		if o.MySQLDamengTrigger != nil {
			require.NoError(t, target.CreateMySQLDamengTrigger(ctx, writer, *o.MySQLDamengTrigger))
		}
	}
	for _, statement := range []string{"INSERT INTO %s VALUES (2,NULL,0)", "INSERT INTO %s VALUES (3,50,0)", "UPDATE %s SET Amount=20 WHERE Id=2", "UPDATE %s SET Amount=30 WHERE Id=3", "DELETE FROM %s WHERE Id=2"} {
		execSource(fmt.Sprintf(statement, sq(orders)))
		// Target columns are quoted uppercase, while these unquoted identifiers
		// resolve to that same actual mapped schema.
		_, err = dst.ExecContext(ctx, fmt.Sprintf(statement, dq(orders)))
		require.NoError(t, err, statement)
	}
	_, err = src.ExecContext(ctx, "UPDATE "+sq(orders)+" SET Amount=-1 WHERE Id=3")
	require.ErrorContains(t, err, "negative amount")
	_, err = dst.ExecContext(ctx, "UPDATE "+dq(orders)+" SET Amount=-1 WHERE Id=3")
	require.ErrorContains(t, err, "negative amount")
	read := func(db *sql.DB, table string, auditing bool) []string {
		t.Helper()
		columns, order := "Id,Amount,Hits", "Id"
		if auditing {
			columns, order = "OrderId,Evt,Hits,Amount", "OrderId,Evt"
		}
		rows, e := db.QueryContext(ctx, "SELECT "+columns+" FROM "+table+" ORDER BY "+order)
		require.NoError(t, e)
		defer rows.Close()
		var values []string
		number := func(s string) string {
			t.Helper()
			n, e := decimal.NewFromString(s)
			require.NoError(t, e)
			return n.String()
		}
		for rows.Next() {
			if auditing {
				var id, hits int
				var evt, amount string
				require.NoError(t, rows.Scan(&id, &evt, &hits, &amount))
				values = append(values, fmt.Sprintf("%d:%s:%d:%s", id, evt, hits, number(amount)))
			} else {
				var id, hits int
				var amount string
				require.NoError(t, rows.Scan(&id, &amount, &hits))
				values = append(values, fmt.Sprintf("%d:%s:%d", id, number(amount), hits))
			}
		}
		require.NoError(t, rows.Err())
		return values
	}
	require.Equal(t, read(src, sq(orders), false), read(dst, dq(orders), false))
	require.Equal(t, read(src, sq(audit), true), read(dst, dq(audit), true))
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT Hits FROM "+dq(orders)+" WHERE Id=3").Scan(&count))
	require.Equal(t, 10, count, "zero-row SELECT INTO must preserve local default")
	// A later data-only request must reject active business triggers before
	// writing, even though it never opts into creating new triggers.
	guardErr := target.CheckMySQLDamengLoadTriggers(ctx, writer, strings.ToUpper(orders))
	require.ErrorContains(t, guardErr, "触发器")
}
