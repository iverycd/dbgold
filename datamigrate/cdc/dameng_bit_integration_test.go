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
	"dbgold/datamigrate/valueconv"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestDamengBit64SnapshotBinlogIntegration(t *testing.T) {
	mysqlDSN, dmDSN := os.Getenv("CDC_MYSQL_DSN"), os.Getenv("CDC_DAMENG_DSN")
	if os.Getenv("CDC_DAMENG_INTEGRATION") != "1" || mysqlDSN == "" || dmDSN == "" {
		t.Skip("requires disposable MySQL and DM instances")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	sourceCfg, err := mysqlDriver.ParseDSN(mysqlDSN)
	require.NoError(t, err)
	sourceCfg.DBName = ""
	admin, err := sql.Open("mysql", sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	database := fmt.Sprintf("cdc_bit_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+database+"`")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := admin.Exec("DROP DATABASE `" + database + "`"); require.NoError(t, e) })
	sourceCfg.DBName = database
	src, err := OpenSource(sourceCfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, src.Close()) })
	_, err = src.ExecContext(ctx, "CREATE TABLE Events (Id INT, Bits BIT(64)) ENGINE=InnoDB")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO Events VALUES(1,18446744073709551615),(2,9223372036854775808)")
	require.NoError(t, err)
	dst, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dst.Close()) })
	schema := fmt.Sprintf("CDC_BIT_%d", time.Now().UnixNano())
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := dst.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	q := quoteIdent(schema) + `."EVENTS"`
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+q+` ("ID" INT,"BITS" NUMBER(20))`)
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+quoteIdent(schema)+`."NARROW" ("ID" INT,"BITS" NUMBER(1))`)
	require.NoError(t, err)
	reader, err := source.NewMySQLForDameng(sourceCfg.FormatDSN(), database, source.ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	position, err := reader.CaptureConsistentSnapshot(ctx)
	require.NoError(t, err)
	columns, types, rows, err := reader.ReadPage(ctx, "Events", nil, 0, 100)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "18446744073709551615", valueconv.NewDaMeng().Convert(rows[0][1], "mysql", "BIT"))
	writer, err := targetwriter.NewDaMeng(dmDSN, schema, targetwriter.ConnPoolConfig{})
	require.NoError(t, err)
	writer.SetSourceType("mysql")
	require.Error(t, writer.CopyData(ctx, "Narrow", columns, types, rows), "NUMBER(1) overflow must fail instead of storing a wrapped negative")
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(schema)+`."NARROW"`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, writer.CopyData(ctx, "Events", columns, types, rows))
	require.NoError(t, writer.Close())
	require.NoError(t, reader.FinishSnapshot())
	var snapshot string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("BITS" AS VARCHAR(30)) FROM `+q+` WHERE "ID"=1`).Scan(&snapshot))
	require.Equal(t, "18446744073709551615", snapshot)
	host, portText, err := net.SplitHostPort(sourceCfg.Addr)
	require.NoError(t, err)
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	cfg := Config{JobID: "bit64", SourceDSN: sourceCfg.FormatDSN(), SourceHost: host, SourcePort: uint16(port), SourceUser: sourceCfg.User, SourcePassword: sourceCfg.Passwd, SourceDatabase: database, TargetDBType: "dameng", TargetDSN: dmDSN, TargetSchema: schema, ServerID: uint32(100000 + time.Now().UnixNano()%1000000000), Start: Position{File: position.File, Pos: position.Pos, GTID: position.GTID}, TableNames: []string{"Events"}, LocatorStrategyVersion: LocatorStrategyVersion}
	tables, err := LoadConfiguredTables(ctx, src, cfg)
	require.NoError(t, err)
	tables, err = ResolveLocatorStrategies(ctx, cfg, tables)
	require.NoError(t, err)
	require.Equal(t, LocatorFullRow, tables[0].LocatorStrategy)
	cfg.LocatorStrategies = LocatorStrategiesFromTables(tables)
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	go func() {
		done <- NewRunner(cfg, Hooks{Stats: func(s Stats) {
			if s.CaughtUp {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}}).Run(runCtx)
	}()
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("BIT runner did not stop")
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
	tx, err := src.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE Events SET Bits=9223372036854775809 WHERE Id=1")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM Events WHERE Id=2")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "INSERT INTO Events VALUES(3,18446744073709551615)")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM Events WHERE Id=3")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	boundary, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	a, err := newDamengApplier(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.Eventually(t, func() bool { p, ok, e := a.LoadCheckpoint(ctx); return e == nil && ok && PositionReached(p, boundary) }, 10*time.Second, 25*time.Millisecond)
	var sourceValue, targetValue string
	require.NoError(t, src.QueryRowContext(ctx, "SELECT CAST(Bits AS UNSIGNED) FROM Events WHERE Id=1").Scan(&sourceValue))
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("BITS" AS VARCHAR(30)) FROM `+q+` WHERE "ID"=1`).Scan(&targetValue))
	require.Equal(t, "9223372036854775809", sourceValue)
	require.Equal(t, sourceValue, targetValue)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count))
	require.Equal(t, 1, count)
}
