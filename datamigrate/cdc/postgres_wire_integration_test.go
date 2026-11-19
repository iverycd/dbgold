//go:build integration

package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func postgresWireFixture(t *testing.T, fileMode bool) (context.Context, *sql.DB, *sql.DB, Config) {
	t.Helper()
	if os.Getenv("CDC_INTEGRATION") != "1" {
		t.Skip("requires disposable MySQL and PostgreSQL CDC fixtures")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	t.Cleanup(cancel)
	src := mustOpenIntegrationDB(t, "mysql", integrationSourceAdminDSN)
	dst := mustOpenIntegrationDB(t, "postgres", integrationTargetDSN)
	t.Cleanup(func() {
		_ = src.Close()
		_ = dst.Close()
	})
	table := fmt.Sprintf("wire_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = src.Exec("DROP TABLE IF EXISTS `" + table + "`")
		_, _ = dst.Exec("DROP SCHEMA IF EXISTS " + quoteIdent(table) + " CASCADE")
	})
	qualified := quoteIdent(table) + "." + quoteIdent(table)
	mustExecIntegration(t, src, "CREATE TABLE "+table+" (id INT PRIMARY KEY, a INT, b INT, state ENUM('new','done'), flags SET('x','y','z'), n TINYINT UNSIGNED, big BIGINT UNSIGNED, note TEXT)")
	mustExecIntegration(t, dst, "CREATE SCHEMA "+quoteIdent(table))
	mustExecIntegration(t, dst, "CREATE TABLE "+qualified+" (id int PRIMARY KEY, a int, b int, state varchar(255), flags text, n int, big numeric(20,0), note text)")
	mustExecIntegration(t, src, "INSERT INTO "+table+" VALUES (1,1,2,'new','x',1,1,NULL)")
	mustExecIntegration(t, dst, "INSERT INTO "+qualified+" VALUES (1,1,2,'new','x',1,1,NULL)")
	start, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	if fileMode {
		start.GTID = ""
	}
	cfg := Config{JobID: table, SourceDSN: integrationSourceCDCDSN, SourceHost: "127.0.0.1", SourcePort: 13306, SourceUser: "cdc", SourcePassword: "cdcpass",
		SourceDatabase: "cdc_source", TargetDSN: integrationTargetDSN, TargetDBType: "postgres", TargetSchema: table,
		TableNames: []string{table}, ServerID: uint32(2000000000 + time.Now().UnixNano()%1000000000), Start: start, LocatorStrategyVersion: LocatorStrategyVersion}
	cfg.SourceSchema, err = CaptureSourceSchema(ctx, cfg, start)
	require.NoError(t, err)
	tables, err := cfg.SourceSchema.TableInfos(cfg.SourceDatabase, cfg.TableNames)
	require.NoError(t, err)
	tables, err = ResolveLocatorStrategies(ctx, cfg, tables)
	require.NoError(t, err)
	cfg.LocatorStrategies = LocatorStrategiesFromTables(tables)
	return ctx, src, dst, cfg
}

func runPostgresWireToHead(t *testing.T, ctx context.Context, src *sql.DB, cfg Config) Position {
	t.Helper()
	head, err := CurrentPosition(ctx, src)
	require.NoError(t, err)
	if cfg.Start.GTID == "" {
		head.GTID = ""
	}
	registered, err := Registry.Register(cfg.JobID)
	require.NoError(t, err)
	defer Registry.Remove(cfg.JobID)
	require.True(t, Registry.RequestCutover(cfg.JobID, head))
	runCtx, stop := context.WithTimeout(registered, 10*time.Second)
	defer stop()
	require.ErrorIs(t, NewRunner(cfg, Hooks{}).Run(runCtx), ErrCutoverReady)
	checkpoint, exists, err := LoadTargetCheckpoint(ctx, cfg)
	require.NoError(t, err)
	require.True(t, exists)
	require.True(t, PositionEquivalent(checkpoint, head), "checkpoint=%+v head=%+v", checkpoint, head)
	return checkpoint
}

func TestPostgresWireSchemaAndValuesIntegration(t *testing.T) {
	for _, mode := range []string{"gtid", "file"} {
		t.Run(mode, func(t *testing.T) {
			ctx, src, dst, cfg := postgresWireFixture(t, mode == "file")
			table := cfg.TableNames[0]
			qualified := quoteIdent(cfg.TargetSchema) + "." + quoteIdent(table)
			runPostgresWireToHead(t, ctx, src, cfg)
			mustExecIntegration(t, src, "UPDATE "+table+" SET a=11,b=22,state='done',flags='x,z',n=200,big=18446744073709551615 WHERE id=1")
			mustExecIntegration(t, src, "ALTER TABLE "+table+" MODIFY b INT AFTER id")
			var blocked Position
			runCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			require.ErrorIs(t, NewRunner(cfg, Hooks{DDL: func(_ string, pos Position) { blocked = pos }}).Run(runCtx), ErrDDLPause)
			stop()
			var a, b, n int
			var state, flags, big string
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT a,b,state,flags,n,big FROM "+qualified+" WHERE id=1").Scan(&a, &b, &state, &flags, &n, &big))
			require.Equal(t, 11, a)
			require.Equal(t, 22, b)
			require.Equal(t, "done", state)
			require.Equal(t, "x,z", flags)
			require.Equal(t, 200, n)
			require.Equal(t, "18446744073709551615", big)
			before, found, err := LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, found)
			require.False(t, PositionReached(before, blocked))

			mustExecIntegration(t, dst, "ALTER TABLE "+qualified+" RENAME COLUMN b TO b_temp")
			_, err = AcknowledgeDDLRecord(ctx, cfg, blocked)
			require.ErrorContains(t, err, "不兼容")
			after, _, err := LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, PositionEquivalent(before, after))
			mustExecIntegration(t, dst, "ALTER TABLE "+qualified+" RENAME COLUMN b_temp TO b")
			record, err := AcknowledgeDDLRecord(ctx, cfg, blocked)
			require.NoError(t, err)
			require.Equal(t, []string{"id", "b", "a"}, record.SourceSchema.Tables[0].Columns[:3])
			retried, err := AcknowledgeDDLRecord(ctx, cfg, blocked)
			require.NoError(t, err)
			require.Equal(t, record, retried)
			mustExecIntegration(t, src, "UPDATE "+table+" SET a=33,b=44 WHERE id=1")
			runPostgresWireToHead(t, ctx, src, cfg)
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT a,b FROM "+qualified+" WHERE id=1").Scan(&a, &b))
			require.Equal(t, 33, a)
			require.Equal(t, 44, b)
		})
	}
}

func TestPostgresWireStopsOnUnsupportedEventsIntegration(t *testing.T) {
	for _, unsupported := range []string{"compressed", "xa", "partial"} {
		t.Run(unsupported, func(t *testing.T) {
			ctx, src, dst, cfg := postgresWireFixture(t, false)
			table := cfg.TableNames[0]
			before := runPostgresWireToHead(t, ctx, src, cfg)
			writer, err := src.Conn(ctx)
			require.NoError(t, err)
			defer writer.Close()
			exec := func(query string) {
				t.Helper()
				_, err := writer.ExecContext(ctx, query)
				require.NoError(t, err)
			}
			var failure string
			switch unsupported {
			case "compressed":
				exec("SET SESSION binlog_transaction_compression=ON")
				exec("INSERT INTO " + table + " (id,a,note) VALUES (2,99,REPEAT('x',2000))")
				exec("SET SESSION binlog_transaction_compression=OFF")
				failure = "TRANSACTION_PAYLOAD_EVENT"
			case "xa":
				id := "'" + table + "'"
				exec("XA START " + id)
				exec("INSERT INTO " + table + " (id,a) VALUES (2,99)")
				exec("XA END " + id)
				exec("XA PREPARE " + id)
				defer func() { _, _ = writer.ExecContext(ctx, "XA ROLLBACK "+id) }()
				failure = "XA"
			case "partial":
				exec("SET SESSION binlog_row_image=MINIMAL")
				exec("UPDATE " + table + " SET a=99 WHERE id=1")
				exec("SET SESSION binlog_row_image=FULL")
				failure = "binlog_row_image=FULL"
			}
			mustExecIntegration(t, src, "INSERT INTO "+table+" (id,a) VALUES (3,99)")
			runCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			err = NewRunner(cfg, Hooks{}).Run(runCtx)
			require.ErrorContains(t, err, failure)
			require.ErrorContains(t, err, "原检查点保留")
			after, found, err := LoadTargetCheckpoint(ctx, cfg)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, PositionEquivalent(before, after))
			var count, value int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*),max(a) FROM "+quoteIdent(cfg.TargetSchema)+"."+quoteIdent(table)).Scan(&count, &value))
			require.Equal(t, 1, count)
			require.Equal(t, 1, value)
		})
	}
}

func TestPostgresWireFileRotateAndManualTransactionBoundaryIntegration(t *testing.T) {
	ctx, src, dst, cfg := postgresWireFixture(t, true)
	before := runPostgresWireToHead(t, ctx, src, cfg)
	mustExecIntegration(t, src, "FLUSH BINARY LOGS")
	after := runPostgresWireToHead(t, ctx, src, cfg)
	require.NotEqual(t, before.File, after.File)
	mustExecIntegration(t, src, "INSERT INTO "+cfg.TableNames[0]+" (id,a) VALUES (2,22),(3,33)")
	rows, err := src.QueryContext(ctx, fmt.Sprintf("SHOW BINLOG EVENTS IN '%s' FROM %d", strings.ReplaceAll(after.File, "'", "''"), after.Pos))
	require.NoError(t, err)
	defer rows.Close()
	var midTransaction Position
	for rows.Next() {
		var file, kind, info string
		var pos, serverID, end uint32
		require.NoError(t, rows.Scan(&file, &pos, &kind, &serverID, &end, &info))
		if strings.EqualFold(kind, "Table_map") {
			midTransaction = Position{File: file, Pos: pos}
			break
		}
	}
	require.NoError(t, rows.Close())
	require.NotEmpty(t, midTransaction.File)
	_, err = CaptureSourceSchema(ctx, cfg, midTransaction)
	require.ErrorContains(t, err, "事务内部")
	runPostgresWireToHead(t, ctx, src, cfg)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteIdent(cfg.TargetSchema)+"."+quoteIdent(cfg.TableNames[0])).Scan(&count))
	require.Equal(t, 3, count)
}
