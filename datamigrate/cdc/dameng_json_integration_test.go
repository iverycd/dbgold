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

	"dbgold/datamigrate/source"
	targetwriter "dbgold/datamigrate/target"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestDamengJSONBinlogIntegration(t *testing.T) {
	mysqlDSN, dmDSN := os.Getenv("CDC_MYSQL_DSN"), os.Getenv("CDC_DAMENG_DSN")
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" || mysqlDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM instances")
	}
	for _, unsupported := range []string{"supported_opaque", "partial_json", "minimal_image"} {
		t.Run(unsupported, func(t *testing.T) { runDamengJSONBinlogTest(t, mysqlDSN, dmDSN, unsupported) })
	}
}

func runDamengJSONBinlogTest(t *testing.T, mysqlDSN, dmDSN, unsupported string) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	sourceCfg, err := mysqlDriver.ParseDSN(mysqlDSN)
	require.NoError(t, err)
	sourceCfg.DBName = ""
	admin, err := sql.Open("mysql", sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	database := fmt.Sprintf("cdc_json_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+database+"` CHARACTER SET utf8mb4")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := admin.Exec("DROP DATABASE `" + database + "`"); require.NoError(t, e) })
	sourceCfg.DBName = database
	src, err := OpenSource(sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	_, err = src.ExecContext(ctx, "CREATE TABLE Events (Id INT, Doc JSON) ENGINE=InnoDB")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "CREATE TABLE Excluded (Id INT, Doc JSON) ENGINE=InnoDB")
	require.NoError(t, err)
	initial := `JSON_OBJECT('long_key', JSON_ARRAY(CAST(1 AS DOUBLE),JSON_OBJECT('z','中文','aa','nested')), 'a',CAST('true' AS JSON),'double',CAST(1 AS DOUBLE),'payload',REPEAT('payload',500),
'opaque',JSON_OBJECT('01',JSON_ARRAY(CAST('123.4500' AS DECIMAL(12,4)),JSON_OBJECT(CONVERT(CONCAT('quoted',CHAR(34),CHAR(92),'key') USING utf8mb4),CAST('-12345678901234567890123456789012345.123456789012345678901234567890' AS DECIMAL(65,30))))),
'date',CAST('2024-01-02' AS DATE),'datetime',CAST('2024-01-02 03:04:05.123456' AS DATETIME(6)),'negative_time',CAST('-25:22:33.123456' AS TIME(6)),'zero_time',CAST('00:00:00' AS TIME(6)),
'long_time',CAST('-111:22:33.123456' AS TIME(6)),'range_time',CAST('838:59:58.999999' AS TIME(6)),
'zero_date',CAST('0000-00-00' AS DATE),'zero_datetime',CAST('0000-00-00 00:00:00' AS DATETIME(6)),
'partial_date',CAST('2024-00-02' AS DATE),'invalid_date',CAST('2024-02-31' AS DATE),'invalid_datetime',CAST('2024-02-31 03:04:05.123456' AS DATETIME(6)))`
	// Historical JSON temporal values can outlive the session sql_mode that
	// allowed them. CDC must preserve their JSON text under today's strict mode.
	seed, err := src.Conn(ctx)
	require.NoError(t, err)
	var originalMode string
	require.NoError(t, seed.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&originalMode))
	_, err = seed.ExecContext(ctx, "SET SESSION sql_mode='ALLOW_INVALID_DATES'")
	require.NoError(t, err)
	_, err = seed.ExecContext(ctx, "INSERT INTO Events VALUES(1,"+initial+"),(2,"+initial+")")
	require.NoError(t, err)
	_, err = seed.ExecContext(ctx, "SET SESSION sql_mode=?", originalMode)
	require.NoError(t, err)
	require.NoError(t, seed.Close())
	dst, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dst.Close()) })
	schema := fmt.Sprintf("CDC_JSON_%d", time.Now().UnixNano())
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := dst.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	q := quoteIdent(schema) + `."EVENTS"`
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+q+` ("ID" INT,"DOC" CLOB)`)
	require.NoError(t, err)
	// Use the real full-snapshot reader and writer. In particular, do not
	// pre-normalize snapshot JSON to the binlog decoder's compact rendering.
	reader, err := source.NewMySQLForDameng(sourceCfg.FormatDSN(), database, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	position, err := reader.CaptureConsistentSnapshot(ctx)
	require.NoError(t, err)
	columns, types, rows, err := reader.ReadPage(ctx, "Events", nil, 0, 100)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	writer, err := targetwriter.NewDaMeng(dmDSN, schema, targetwriter.ConnPoolConfig{})
	require.NoError(t, err)
	writer.SetSourceType("mysql")
	require.NoError(t, writer.CopyData(ctx, "Events", columns, types, rows))
	require.NoError(t, writer.Close())
	require.NoError(t, reader.FinishSnapshot())
	host, portText, err := net.SplitHostPort(sourceCfg.Addr)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	cfg := Config{JobID: "json", SourceDSN: sourceCfg.FormatDSN(), SourceHost: host, SourcePort: uint16(port), SourceUser: sourceCfg.User, SourcePassword: sourceCfg.Passwd, SourceDatabase: database, TargetDBType: "dameng", TargetDSN: dmDSN, TargetSchema: schema, Mode: "all", ServerID: uint32(100000 + time.Now().UnixNano()%1000000000), Start: Position{File: position.File, Pos: position.Pos, GTID: position.GTID}, TableNames: []string{"Events"}, LocatorStrategyVersion: LocatorStrategyVersion}
	tables, err := LoadConfiguredTables(ctx, src, cfg)
	require.NoError(t, err)
	tables, err = ResolveLocatorStrategies(ctx, cfg, tables)
	require.NoError(t, err)
	cfg.LocatorStrategies = LocatorStrategiesFromTables(tables)
	cfg.SourceSchema, err = NewSourceSchema(cfg.SourceDatabase, tables)
	require.NoError(t, err)
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	runner := NewRunner(cfg, Hooks{Stats: func(s Stats) {
		if s.CaughtUp {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}})
	runCtx, stop := context.WithCancel(ctx)
	go func() { done <- runner.Run(runCtx) }()
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("JSON runner did not stop")
			}
		}
	}()
	select {
	case <-ready:
	case e := <-done:
		joined = true
		t.Fatalf("runner failed before ready: %v", e)
	case <-ctx.Done():
		t.Fatal("runner did not become ready")
	}
	a, err := newDamengApplier(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	// Unselected JSON values and partial-update events must not block this task.
	_, err = src.ExecContext(ctx, `INSERT INTO Excluded VALUES(1,JSON_OBJECT('n',CAST('123.4500' AS DECIMAL(12,4))))`)
	require.NoError(t, err)
	excluded, err := src.Conn(ctx)
	require.NoError(t, err)
	_, err = excluded.ExecContext(ctx, `SET SESSION binlog_row_value_options='PARTIAL_JSON'`)
	require.NoError(t, err)
	_, err = excluded.ExecContext(ctx, `UPDATE Excluded SET Doc=JSON_SET(Doc,'$.n',1) WHERE Id=1`)
	require.NoError(t, err)
	_, err = excluded.ExecContext(ctx, `SET SESSION binlog_row_value_options=''`)
	require.NoError(t, err)
	require.NoError(t, excluded.Close())
	tx, err := src.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE Events SET Doc=JSON_SET(Doc,'$.long_key[1].z','updated中文') WHERE Id=1`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM Events WHERE Id=2")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "INSERT INTO Events SELECT 3,Doc FROM Events WHERE Id=1")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM Events WHERE Id=3")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	boundary, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	require.Eventually(t, func() bool { p, ok, e := a.LoadCheckpoint(ctx); return e == nil && ok && PositionReached(p, boundary) }, 10*time.Second, 25*time.Millisecond)
	var sourceJSON, targetJSON, numberType string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT Doc,JSON_TYPE(JSON_EXTRACT(Doc,'$.double')) FROM Events WHERE Id=1`).Scan(&sourceJSON, &numberType))
	require.Equal(t, "DOUBLE", numberType)
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("DOC" AS VARCHAR(8000)) FROM `+q+` WHERE "ID"=1`).Scan(&targetJSON))
	require.Equal(t, sourceJSON, targetJSON)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count))
	require.Equal(t, 1, count)
	if unsupported == "supported_opaque" {
		_, err = src.ExecContext(ctx, `INSERT INTO Events VALUES(9,JSON_EXTRACT(JSON_OBJECT('n',CAST('123.4500' AS DECIMAL(12,4))),'$.n'))`)
		require.NoError(t, err)
		var typ string
		require.NoError(t, src.QueryRowContext(ctx, `SELECT JSON_TYPE(Doc) FROM Events WHERE Id=9`).Scan(&typ))
		require.Equal(t, "DECIMAL", typ)
		boundary, err = CurrentPosition(ctx, src)
		require.NoError(t, err)
		require.Eventually(t, func() bool { p, ok, e := a.LoadCheckpoint(ctx); return e == nil && ok && PositionReached(p, boundary) }, 10*time.Second, 25*time.Millisecond)
		var rootDecimal string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("DOC" AS VARCHAR(8000)) FROM `+q+` WHERE "ID"=9`).Scan(&rootDecimal))
		require.Equal(t, "123.4500", rootDecimal)
		_, err = src.ExecContext(ctx, `UPDATE Events SET Doc=JSON_EXTRACT(JSON_OBJECT('n',CAST('12345678901234567890123456789012345.123456789012345678901234567890' AS DECIMAL(65,30))),'$.n') WHERE Id=9`)
		require.NoError(t, err)
		boundary, err = CurrentPosition(ctx, src)
		require.NoError(t, err)
		require.Eventually(t, func() bool { p, ok, e := a.LoadCheckpoint(ctx); return e == nil && ok && PositionReached(p, boundary) }, 10*time.Second, 25*time.Millisecond)
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("DOC" AS VARCHAR(8000)) FROM `+q+` WHERE "ID"=9`).Scan(&rootDecimal))
		require.Equal(t, "12345678901234567890123456789012345.123456789012345678901234567890", rootDecimal)
		_, err = src.ExecContext(ctx, "DELETE FROM Events WHERE Id=9")
		require.NoError(t, err)
		boundary, err = CurrentPosition(ctx, src)
		require.NoError(t, err)
		require.Eventually(t, func() bool { p, ok, e := a.LoadCheckpoint(ctx); return e == nil && ok && PositionReached(p, boundary) }, 10*time.Second, 25*time.Millisecond)
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count))
		require.Equal(t, 1, count)
		return
	} else if unsupported == "partial_json" {
		conn, err := src.Conn(ctx)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `SET SESSION binlog_row_value_options='PARTIAL_JSON'`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `UPDATE Events SET Doc=JSON_SET(Doc,'$.long_key[1].z','x') WHERE Id=1`)
		require.NoError(t, err)
		_, resetErr := conn.ExecContext(ctx, `SET SESSION binlog_row_value_options=''`)
		require.NoError(t, resetErr)
		require.NoError(t, conn.Close())
	} else {
		conn, err := src.Conn(ctx)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `SET SESSION binlog_row_image='MINIMAL'`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `UPDATE Events SET Id=10 WHERE Id=1`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `SET SESSION binlog_row_image='FULL'`)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}
	failedBoundary, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	select {
	case e := <-done:
		joined = true
		require.Error(t, e)
		if unsupported == "partial_json" {
			require.ErrorContains(t, e, "PARTIAL_JSON")
		} else {
			require.ErrorContains(t, e, "binlog_row_image=FULL")
		}
	case <-ctx.Done():
		t.Fatal("unsupported JSON did not stop replication")
	}
	cp, exists, err := a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, exists)
	require.False(t, PositionReached(cp, failedBoundary), "unsupported JSON must not advance its checkpoint")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("DOC" AS VARCHAR(8000)) FROM `+q+` WHERE "ID"=1`).Scan(&targetJSON))
	require.Equal(t, sourceJSON, targetJSON)
}
