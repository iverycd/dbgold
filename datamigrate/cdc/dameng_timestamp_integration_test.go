package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestDamengTimestampBinlogIntegration(t *testing.T) {
	mysqlDSN, dmDSN := os.Getenv("CDC_MYSQL_DSN"), os.Getenv("CDC_DAMENG_DSN")
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" || mysqlDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM instances")
	}
	for _, zone := range []string{"+08:00", "America/New_York"} {
		for _, local := range []struct {
			name     string
			location *time.Location
		}{{"UTC", time.UTC}, {"Plus13", time.FixedZone("application-local", 13*3600)}} {
			t.Run(zone+"/"+local.name, func(t *testing.T) {
				previous := time.Local
				time.Local = local.location
				defer func() { time.Local = previous }()
				runDamengTimestampBinlogTest(t, mysqlDSN, dmDSN, zone)
			})
		}
	}
}

func runDamengTimestampBinlogTest(t *testing.T, mysqlDSN, dmDSN, zone string) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	sourceCfg, err := mysqlDriver.ParseDSN(mysqlDSN)
	require.NoError(t, err)
	sourceCfg.ParseTime = false
	if sourceCfg.Params == nil {
		sourceCfg.Params = map[string]string{}
	}
	sourceCfg.Params["time_zone"] = "'" + zone + "'"
	sourceCfg.DBName = ""
	admin, err := sql.Open("mysql", sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	if err = admin.PingContext(ctx); err != nil {
		if zone == "America/New_York" {
			t.Skipf("source timezone tables are required for historical DST: %v", err)
		}
		require.NoError(t, err)
	}
	database := fmt.Sprintf("cdc_time_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+database+"`")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := admin.Exec("DROP DATABASE `" + database + "`"); require.NoError(t, e) })
	sourceCfg.DBName = database
	src, err := OpenSource(sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	_, err = src.ExecContext(ctx, "CREATE TABLE Events (Id INT, Stamp TIMESTAMP(6), Wall DATETIME(6)) ENGINE=InnoDB")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO Events VALUES (1,'2024-01-02 03:04:05.123456','2024-01-02 03:04:05.123456')")
	require.NoError(t, err)
	dst, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dst.Close()) })
	schema := fmt.Sprintf("CDC_TIME_%d", time.Now().UnixNano())
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := dst.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	target := quoteIdent(schema) + `."EVENTS"`
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+target+` ("ID" INT,"STAMP" TIMESTAMP(6),"WALL" TIMESTAMP(6))`)
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "INSERT INTO "+target+` VALUES (1,TO_TIMESTAMP('2024-01-02 03:04:05.123456','YYYY-MM-DD HH24:MI:SS.FF6'),TO_TIMESTAMP('2024-01-02 03:04:05.123456','YYYY-MM-DD HH24:MI:SS.FF6'))`)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(sourceCfg.Addr)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	start, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	cfg := Config{JobID: "timestamp", SourceDSN: sourceCfg.FormatDSN(), SourceHost: host, SourcePort: uint16(port), SourceUser: sourceCfg.User, SourcePassword: sourceCfg.Passwd, SourceDatabase: database, TargetDBType: "dameng", TargetDSN: dmDSN, TargetSchema: schema, Mode: "all", ServerID: uint32(100000 + time.Now().UnixNano()%1000000000), Start: start, TableNames: []string{"Events"}, LocatorStrategyVersion: LocatorStrategyVersion}
	tables, err := LoadConfiguredTables(ctx, src, cfg)
	require.NoError(t, err)
	tables, err = ResolveLocatorStrategies(ctx, cfg, tables)
	require.NoError(t, err)
	cfg.LocatorStrategies = LocatorStrategiesFromTables(tables)
	ready := make(chan struct{}, 1)
	runner := NewRunner(cfg, Hooks{Stats: func(s Stats) {
		if s.CaughtUp {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("timestamp runner did not stop")
		}
	}()
	select {
	case <-ready:
	case e := <-done:
		t.Fatalf("runner failed before ready: %v", e)
	case <-ctx.Done():
		t.Fatal("runner did not become ready")
	}
	// The January before-image and July after-image have different DST
	// offsets in New York; a fixed offset breaks this keyless UPDATE.
	tx, err := src.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE Events SET Stamp='2024-07-04 12:13:14.654321',Wall='2024-07-04 12:13:14.654321' WHERE Id=1")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "INSERT INTO Events VALUES(2,'2024-11-03 01:15:00.987654','2024-11-03 01:15:00.987654')")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM Events WHERE Id=2")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	boundary, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	a, err := newDamengApplier(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.Eventually(t, func() bool { p, ok, e := a.LoadCheckpoint(ctx); return e == nil && ok && PositionReached(p, boundary) }, 10*time.Second, 25*time.Millisecond)
	var stamp, wall string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT TO_CHAR("STAMP",'YYYY-MM-DD HH24:MI:SS.FF6'),TO_CHAR("WALL",'YYYY-MM-DD HH24:MI:SS.FF6') FROM `+target+` WHERE "ID"=1`).Scan(&stamp, &wall))
	require.Equal(t, "2024-07-04 12:13:14.654321", stamp)
	require.Equal(t, stamp, wall)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+target).Scan(&count))
	require.Equal(t, 1, count)
}
