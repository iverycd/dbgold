package validation

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
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
	testValidationPGRawScanIntegration(t, "VALIDATION_POSTGRES_DSN")
}

func TestValidationKingbaseRawScanIntegration(t *testing.T) {
	testValidationPGRawScanIntegration(t, "VALIDATION_KINGBASE_DSN")
}

func testValidationPGRawScanIntegration(t *testing.T, dsnEnv string) {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("validation_scanner_%d", time.Now().UnixNano())
	quotedSchema := QuoteValidationIdent("postgres", schema)
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema)
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
	table := "MiXeDRows"
	qualified := quotedSchema + "." + QuoteValidationIdent("postgres", table)
	_, err = db.ExecContext(ctx, "CREATE TABLE "+qualified+` ("Id" bigint, "At" timestamptz, "Money" numeric(38,10), "Text" bytea, "Bits" bit(9), "Last" bigint, "Original" varchar(64), PRIMARY KEY ("Last","Id"))`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO "+qualified+` VALUES (9223372036854775807,'2024-10-22 11:12:13.123456+08','12345678901234567890.1234567890',$1,B'000000101',7,$2)`, []byte{'a', 0, 'b', ' '}, "原始文本  ")
	require.NoError(t, err)
	s := NewSQLScanner(db, "postgres", schema)
	meta, err := s.DescribeValidationTable(ctx, table)
	require.NoError(t, err)
	require.Equal(t, []string{"Last", "Id"}, meta.PrimaryKey)
	require.Len(t, meta.Columns, 7)
	require.Equal(t, "Original", meta.Columns[6].Name)
	require.Equal(t, int64(38), meta.Columns[2].Precision)
	require.Equal(t, int64(10), meta.Columns[2].Scale)
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
	require.Equal(t, "7", row[5])
	require.Equal(t, "原始文本  ", row[6])
}

func TestValidationPostgresBitsIntegration(t *testing.T) {
	testValidationPGBitsIntegration(t, "VALIDATION_POSTGRES_DSN")
}

func TestValidationKingbaseBitsIntegration(t *testing.T) {
	testValidationPGBitsIntegration(t, "VALIDATION_KINGBASE_DSN")
}

func testValidationPGBitsIntegration(t *testing.T, dsnEnv string) {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	// Keep the Kingbase display setting and scanner on one physical session.
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("validation_bits_%d", time.Now().UnixNano())
	quotedSchema := QuoteValidationIdent("postgres", schema)
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema)
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
	table := "BitRows"
	qualified := quotedSchema + "." + QuoteValidationIdent("postgres", table)
	_, err = db.ExecContext(ctx, "CREATE TABLE "+qualified+` ("Id" bigint PRIMARY KEY, "One" bit(1), "Nine" bit(9), "Wide" bit(64), "Variable" bit varying(17), "__dbgold_bits" bit(9), "__dbgold_bit_index" bit(9), "Bit""Quote" bit(9))`)
	require.NoError(t, err)
	expected := [][]any{
		{"1", "0", "000100101", "1" + strings.Repeat("0", 60) + "101", "00101", "100001001", "010010001", "001000101"},
		{"2", "1", "101001000", strings.Repeat("1", 64), "10000000000000001", "001101000", "000110001", "110000001"},
		{"3", nil, nil, nil, nil, nil, nil, nil},
	}
	for _, row := range expected {
		literals := []string{row[0].(string)}
		for _, value := range row[1:] {
			literal := "NULL"
			if value != nil {
				literal = "B'" + value.(string) + "'"
			}
			literals = append(literals, literal)
		}
		_, err = db.ExecContext(ctx, "INSERT INTO "+qualified+" VALUES ("+strings.Join(literals, ",")+")")
		require.NoError(t, err)
	}
	scanner := NewSQLScanner(db, "postgres", schema)
	meta, err := scanner.DescribeValidationTable(ctx, table)
	require.NoError(t, err)
	require.Len(t, meta.Columns, 8)
	for _, column := range meta.Columns[1:] {
		require.Equal(t, Bits, column.Kind, column.Name)
	}
	assertScan := func(want [][]any) {
		stream, err := scanner.OpenValidationScan(ctx, table, meta.Columns)
		require.NoError(t, err)
		defer stream.Close()
		actual := map[string][]any{}
		for {
			row, err := stream.Next(ctx)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			id, err := scalarText(row[0])
			require.NoError(t, err)
			require.NotContains(t, actual, id)
			actual[id] = row
		}
		require.NoError(t, stream.Close())
		require.Len(t, actual, len(want))
		for _, row := range want {
			require.Equal(t, row, actual[row[0].(string)])
		}
	}
	validate := func() TableResult {
		// Use known bit strings as the source so matching SQL serialization bugs
		// cannot make both sides agree on an incorrect value.
		source := &memoryScanner{meta: meta, rows: expected}
		return Validate(ctx, source, scanner, Plan{SourceTable: "Original", TargetTable: table})
	}
	modes := []string{""}
	if dsnEnv == "VALIDATION_KINGBASE_DSN" {
		var original string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT current_setting('bit_output')").Scan(&original))
		defer func() {
			_, err := db.ExecContext(context.Background(), "SELECT set_config('bit_output',$1,false)", original)
			require.NoError(t, err)
		}()
		modes = []string{"escape", "hex", "bin"}
	}
	for _, mode := range modes {
		if mode != "" {
			_, err = db.ExecContext(ctx, "SELECT set_config('bit_output',$1,false)", mode)
			require.NoError(t, err)
		}
		assertScan(expected)
		result := validate()
		require.Equal(t, StatusPassed, result.Status, result.Error)
		require.EqualValues(t, 3, result.SourceRows)
		require.EqualValues(t, 3, result.TargetRows)
		_, err = db.ExecContext(ctx, "UPDATE "+qualified+` SET "One"=B'1' WHERE "Id"=1`)
		require.NoError(t, err)
		result = validate()
		require.Equal(t, StatusMismatch, result.Status, result.Error)
		require.EqualValues(t, 3, result.SourceRows)
		require.EqualValues(t, 3, result.TargetRows)
		require.EqualValues(t, 1, result.ChangedRows)
		require.Zero(t, result.MissingRows)
		require.Zero(t, result.ExtraRows)
		_, err = db.ExecContext(ctx, "UPDATE "+qualified+` SET "One"=B'0' WHERE "Id"=1`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "INSERT INTO "+qualified+` ("Id","Variable") VALUES (4,B'')`)
		require.NoError(t, err)
		assertScan(append(expected, []any{"4", nil, nil, nil, "", nil, nil, nil}))
		result = validate()
		require.Equal(t, StatusUnsupported, result.Status, result.Error)
		require.Contains(t, result.Error, "BIT")
		_, err = db.ExecContext(ctx, "DELETE FROM "+qualified+` WHERE "Id"=4`)
		require.NoError(t, err)
		if mode != "" {
			var actual string
			require.NoError(t, db.QueryRowContext(ctx, "SELECT current_setting('bit_output')").Scan(&actual))
			require.Equal(t, mode, actual, "the scanner must preserve bit_output")
		}
	}
}

func TestValidationPostgresDomainMetadataIntegration(t *testing.T) {
	dsn := os.Getenv("VALIDATION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("VALIDATION_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("validation_domains_%d", time.Now().UnixNano())
	quotedSchema := QuoteValidationIdent("postgres", schema)
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema)
	require.NoError(t, err)
	defer db.ExecContext(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
	qualified := func(name string) string {
		return quotedSchema + "." + QuoteValidationIdent("postgres", name)
	}
	for _, statement := range []string{
		"CREATE TYPE " + qualified("State") + " AS ENUM ('ready','done')",
		"CREATE DOMAIN " + qualified("StateDomain") + " AS " + qualified("State"),
		"CREATE DOMAIN " + qualified("ArrayDomain") + " AS integer[]",
		"CREATE DOMAIN " + qualified("AmountDomain") + " AS numeric(24,6)",
		"CREATE TABLE " + qualified("Rows") + ` ("Id" bigint PRIMARY KEY, "State" ` + qualified("State") + `, "StateDomain" ` + qualified("StateDomain") + `, "Numbers" integer[], "NumbersDomain" ` + qualified("ArrayDomain") + `, "Amount" ` + qualified("AmountDomain") + `)`,
	} {
		_, err = db.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	meta, err := NewSQLScanner(db, "postgres", schema).DescribeValidationTable(ctx, "Rows")
	require.NoError(t, err)
	require.Equal(t, []string{"Id"}, meta.PrimaryKey)
	require.Len(t, meta.Columns, 6)
	// information_schema.udt_name identifies the domain's underlying type.
	// Preserve that behavior while obtaining the name from the system catalog.
	for _, index := range []int{1, 2} {
		require.Equal(t, "State", meta.Columns[index].DatabaseType)
		require.Equal(t, Unsupported, meta.Columns[index].Kind)
	}
	for _, index := range []int{3, 4} {
		require.Equal(t, "_int4", meta.Columns[index].DatabaseType)
		require.Equal(t, Unsupported, meta.Columns[index].Kind)
	}
	require.Equal(t, "numeric", meta.Columns[5].DatabaseType)
	require.Equal(t, Number, meta.Columns[5].Kind)
	require.Equal(t, int64(24), meta.Columns[5].Precision)
	require.Equal(t, int64(6), meta.Columns[5].Scale)
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
