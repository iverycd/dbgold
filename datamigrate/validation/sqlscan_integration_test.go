package validation

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestValidationMySQLRawScanIntegration(t *testing.T) {
	dsn := os.Getenv("VALIDATION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("VALIDATION_MYSQL_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `SET SESSION sql_mode=''`)
	require.NoError(t, err)
	table := fmt.Sprintf("validation_scanner_%d", time.Now().UnixNano())
	_, err = conn.ExecContext(ctx, "CREATE TABLE "+table+" (`Id` BIGINT UNSIGNED PRIMARY KEY, `At` DATETIME(6), `Money` DECIMAL(38,10), `Text` VARBINARY(20), `Bits` BIT(9))")
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table)
	_, err = conn.ExecContext(ctx, "INSERT INTO "+table+` VALUES (18446744073709551615,'2024-00-12 11:12:13.123456','12345678901234567890.1234567890',?,b'000000101')`, []byte{'a', 0, 'b', ' '})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	s := NewSQLScanner(db, "mysql", "")
	meta, err := s.DescribeValidationTable(ctx, table)
	require.NoError(t, err)
	require.Equal(t, []string{"Id"}, meta.PrimaryKey)
	stream, err := s.OpenValidationScan(ctx, table, meta.Columns)
	require.NoError(t, err)
	defer stream.Close()
	row, err := stream.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("18446744073709551615"), row[0])
	require.Equal(t, []byte("2024-00-12 11:12:13.123456"), row[1])
	require.Equal(t, []byte("12345678901234567890.1234567890"), row[2])
	require.Equal(t, []byte{'a', 0, 'b', ' '}, row[3])
	require.Equal(t, []byte("000000101"), row[4])
}
func TestValidationPostgresRawScanIntegration(t *testing.T) {
	dsn := os.Getenv("VALIDATION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("VALIDATION_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	table := fmt.Sprintf("validation_scanner_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE TABLE public."+table+` ("Id" bigint PRIMARY KEY, "At" timestamptz, "Money" numeric(38,10), "Text" bytea, "Bits" bit(9))`)
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS public."+table)
	_, err = db.ExecContext(ctx, "INSERT INTO public."+table+` VALUES (9223372036854775807,'2024-10-22 11:12:13.123456+08','12345678901234567890.1234567890',$1,B'000000101')`, []byte{'a', 0, 'b', ' '})
	require.NoError(t, err)
	s := NewSQLScanner(db, "postgres", "public")
	meta, err := s.DescribeValidationTable(ctx, table)
	require.NoError(t, err)
	require.Equal(t, []string{"Id"}, meta.PrimaryKey)
	stream, err := s.OpenValidationScan(ctx, table, meta.Columns)
	require.NoError(t, err)
	defer stream.Close()
	row, err := stream.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, "9223372036854775807", row[0])
	require.Equal(t, "2024-10-22 03:12:13.123456+00", row[1])
	require.Equal(t, "12345678901234567890.1234567890", row[2])
	require.Equal(t, []byte{'a', 0, 'b', ' '}, row[3])
	require.Equal(t, "000000101", row[4])
}

func TestValidationMySQLTimestampKeepsSessionTimezoneIntegration(t *testing.T) {
	dsn := os.Getenv("VALIDATION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("VALIDATION_MYSQL_DSN is not set")
	}
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+08:00'"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	table := fmt.Sprintf("validation_scanner_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE TABLE "+table+` (Id bigint PRIMARY KEY, At timestamp(6))`)
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table)
	_, err = db.ExecContext(ctx, "INSERT INTO "+table+` VALUES(1,'2024-10-22 11:12:13.123456')`)
	require.NoError(t, err)
	scanner := NewSQLScanner(db, "mysql", "")
	meta, err := scanner.DescribeValidationTable(ctx, table)
	require.NoError(t, err)
	stream, err := scanner.OpenValidationScan(ctx, table, meta.Columns)
	require.NoError(t, err)
	row, err := stream.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("2024-10-22 11:12:13.123456"), row[1])
	require.NoError(t, stream.Close())
	var zone string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT @@SESSION.time_zone`).Scan(&zone))
	require.Equal(t, "+08:00", zone)
}
