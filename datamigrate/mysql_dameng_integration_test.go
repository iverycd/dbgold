package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Opt-in: use a dedicated MySQL test database and a dedicated DM test schema.
// Only the uniquely named fixture table is created/dropped in each database.
func TestMySQLDamengZeroDateIntegration(t *testing.T) {
	srcDSN := os.Getenv("MYSQL_DM_TEST_SOURCE_DSN")
	dstDSN := os.Getenv("MYSQL_DM_TEST_TARGET_DSN")
	schema := os.Getenv("MYSQL_DM_TEST_SCHEMA")
	if srcDSN == "" || dstDSN == "" || schema == "" {
		t.Skip("需要 MYSQL_DM_TEST_SOURCE_DSN / MYSQL_DM_TEST_TARGET_DSN / MYSQL_DM_TEST_SCHEMA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, err := mysql.ParseDSN(srcDSN)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.DBName)
	src, err := sql.Open("mysql", srcDSN)
	require.NoError(t, err)
	src.SetMaxOpenConns(1)
	src.SetMaxIdleConns(1)
	t.Cleanup(func() { src.Close() })
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { dst.Close() })
	table := fmt.Sprintf("DbgZeroDate_%d", time.Now().UnixNano())
	sq := "`" + table + "`"
	dq := dialect.NewDaMeng().QualifyTable(schema, table)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = src.ExecContext(cleanup, "DROP TABLE "+sq)
		_, _ = dst.ExecContext(cleanup, "DROP TABLE "+dq+" CASCADE CONSTRAINTS")
	})
	// Only this fixture connection permits zero dates; source Reader sessions
	// keep their original SQL mode and only read the generated test table.
	_, err = src.ExecContext(ctx, "SET SESSION sql_mode = ''")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "CREATE TABLE "+sq+` (
		Id INT NOT NULL PRIMARY KEY,
		D DATE NOT NULL DEFAULT '0000-00-00',
		At DATETIME(6) NOT NULL DEFAULT '0000-00-00 00:00:00',
		Ts TIMESTAMP(6) NULL DEFAULT NULL,
		Txt VARCHAR(32) NOT NULL DEFAULT '0000-00-00',
		OptionalDt DATETIME(6) NULL
	)`)
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+sq+` VALUES
		(1, '0000-00-00', '0000-00-00 00:00:00', '0000-00-00 00:00:00', '0000-00-00', NULL),
		(2, '2024-00-15', '2024-12-00 12:34:56.123456', '2024-02-29 12:34:56.123456', 'keep', '2024-02-29 12:34:56.123456'),
		(3, '2024-02-29', '2024-02-29 12:34:56.123456', NULL, 'valid', NULL)`)
	require.NoError(t, err)

	for _, mode := range []string{"schema_only", "data_only", "both"} {
		t.Run(mode, func(t *testing.T) {
			reader, err := source.NewMySQLForDameng(srcDSN, cfg.DBName, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer reader.Close()
			writer, err := target.NewDaMeng(dstDSN, schema, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer writer.Close()
			job := &Job{LogCh: make(chan string, 1024)}
			m := NewMigrator(reader, writer, job, Config{
				Mode: "include", Filter: table, Content: mode, TargetSchema: schema,
				TargetDBType: "dameng", PageSize: 2, MaxParallel: 1,
			})
			report := m.Run(ctx)
			require.False(t, report.HasFailures(), "%+v; logs=%v", report, drainLogs(job))
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq).Scan(&count))
			if mode == "schema_only" {
				require.Zero(t, count)
				// Omit dates to exercise the rewritten NOT NULL defaults.
				_, err = dst.ExecContext(ctx, "INSERT INTO "+dq+` ("ID") VALUES (100)`)
				require.NoError(t, err)
				var d, at, text string
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT TO_CHAR("D", 'YYYY-MM-DD'), TO_CHAR("AT", 'YYYY-MM-DD HH24:MI:SS.FF6'), "TXT" FROM `+dq+` WHERE "ID"=100`).Scan(&d, &at, &text))
				require.Equal(t, "1900-01-01", d)
				require.Equal(t, "1900-01-01 00:00:00.000000", at)
				require.Equal(t, "0000-00-00", text)
				_, err = dst.ExecContext(ctx, "DELETE FROM "+dq+` WHERE "ID"=100`)
				require.NoError(t, err)
				return
			}
			require.Equal(t, 3, count)
			rows, err := dst.QueryContext(ctx, `SELECT "ID", TO_CHAR("D", 'YYYY-MM-DD'), TO_CHAR("AT", 'YYYY-MM-DD HH24:MI:SS.FF6'), TO_CHAR("TS", 'YYYY-MM-DD HH24:MI:SS.FF6'), "TXT", TO_CHAR("OPTIONALDT", 'YYYY-MM-DD HH24:MI:SS.FF6') FROM `+dq+` ORDER BY "ID"`)
			require.NoError(t, err)
			defer rows.Close()
			for i := 1; i <= 3; i++ {
				require.True(t, rows.Next())
				var id int
				var d, at, text string
				var ts, optional sql.NullString
				require.NoError(t, rows.Scan(&id, &d, &at, &ts, &text, &optional))
				require.Equal(t, i, id)
				if i < 3 {
					require.Equal(t, "1900-01-01", d)
					require.Equal(t, "1900-01-01 00:00:00.000000", at)
				} else {
					require.Equal(t, "2024-02-29", d)
					require.Equal(t, "2024-02-29 12:34:56.123456", at)
				}
				switch i {
				case 1:
					require.Equal(t, sql.NullString{String: "1900-01-01 00:00:00.000000", Valid: true}, ts)
					require.Equal(t, "0000-00-00", text)
					require.False(t, optional.Valid)
				case 2:
					require.Equal(t, sql.NullString{String: "2024-02-29 12:34:56.123456", Valid: true}, ts)
					require.Equal(t, ts, optional)
				case 3:
					require.False(t, ts.Valid)
					require.False(t, optional.Valid)
				}
			}
			require.False(t, rows.Next())
			require.NoError(t, rows.Err())
		})
	}
}
