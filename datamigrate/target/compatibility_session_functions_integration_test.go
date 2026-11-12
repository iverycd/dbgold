package target

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func checkGetDateSQL(t *testing.T, ctx context.Context, db *sql.DB, schema, product string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()

	call := qualifiedFunction(schema, "getdate") + "()"
	statementStart := "statement_timestamp()"
	if product == "vastbase" {
		// Vastbase V3's native statement_timestamp is fixed at transaction start.
		statementStart = "(SELECT query_start FROM pg_stat_activity WHERE pid = pg_backend_pid())"
	}
	query := "SELECT " + call + ", " + call + " = date_trunc('second', " + statementStart + ")::timestamp without time zone, " +
		"pg_typeof(" + call + ") = pg_typeof(NULL::timestamp without time zone)"
	checkStatementTime := func() time.Time {
		t.Helper()
		var got time.Time
		var correctTime, correctType bool
		require.NoError(t, tx.QueryRowContext(ctx, query).Scan(&got, &correctTime, &correctType))
		require.True(t, correctTime)
		require.True(t, correctType)
		require.Zero(t, got.Nanosecond())
		return time.Date(got.Year(), got.Month(), got.Day(), got.Hour(), got.Minute(), got.Second(), 0, time.UTC)
	}
	_, err = tx.ExecContext(ctx, "SET LOCAL TIME ZONE 'UTC'")
	require.NoError(t, err)
	utc := checkStatementTime()
	_, err = tx.ExecContext(ctx, "SET LOCAL TIME ZONE 'Asia/Shanghai'")
	require.NoError(t, err)
	first := checkStatementTime()
	require.InDelta(t, (8 * time.Hour).Seconds(), first.Sub(utc).Seconds(), 30, "wall time reflects the session timezone")
	time.Sleep(time.Second)
	require.True(t, checkStatementTime().After(first), "later statements in the same transaction use their own start time")
}

func checkGetRowIDSQL(t *testing.T, ctx context.Context, db *sql.DB, schema string) {
	t.Helper()
	first, err := db.Conn(ctx)
	require.NoError(t, err)
	defer first.Close()
	second, err := db.Conn(ctx)
	require.NoError(t, err)
	defer second.Close()

	call := qualifiedFunction(schema, "getrowid") + "()"
	query := "SELECT " + call
	read := func(row *sql.Row) sql.NullInt64 {
		t.Helper()
		var result sql.NullInt64
		require.NoError(t, row.Scan(&result))
		return result
	}
	assertID := func(row *sql.Row, value int64) {
		t.Helper()
		require.Equal(t, sql.NullInt64{Int64: value, Valid: true}, read(row))
	}
	seed := func(conn *sql.Conn, statement string) {
		t.Helper()
		_, err := conn.ExecContext(ctx, statement)
		require.NoError(t, err)
	}

	require.False(t, read(first.QueryRowContext(ctx, query)).Valid)
	seed(first, "SET dbgold.getrowid = '0'")
	assertID(first.QueryRowContext(ctx, query), 1)
	require.False(t, read(second.QueryRowContext(ctx, query)).Valid)
	seed(second, "SET dbgold.getrowid = '10'")
	assertID(second.QueryRowContext(ctx, query), 11)
	assertID(first.QueryRowContext(ctx, query), 2)

	tx, err := first.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	assertID(tx.QueryRowContext(ctx, query), 3)
	require.NoError(t, tx.Rollback())
	assertID(first.QueryRowContext(ctx, query), 3)
	assertID(second.QueryRowContext(ctx, query), 12)

	seed(first, "SET dbgold.getrowid = '0'")
	assertID(first.QueryRowContext(ctx, query), 1)
	seed(first, "SET dbgold.getrowid = '2147483647'")
	var value sql.NullInt64
	require.Error(t, first.QueryRowContext(ctx, query).Scan(&value), "incrementing past the integer range must fail")
	seed(first, "RESET dbgold.getrowid")
	require.False(t, read(first.QueryRowContext(ctx, query)).Valid)
	seed(second, "RESET dbgold.getrowid")
}

func checkIsNullSQL(t *testing.T, ctx context.Context, db *sql.DB, schema, product string) {
	t.Helper()
	query := "SELECT " + qualifiedFunction(schema, "isnull") + "($1::varchar, $2::varchar)"
	fullLength := strings.Repeat("字", 200)
	trailingSpaceBoundary := strings.Repeat("字", 199) + " "
	var empty sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, "SELECT $1::varchar", "").Scan(&empty))
	if product == "postgres" {
		require.Equal(t, sql.NullString{Valid: true}, empty)
	}
	emptyOrFallback := "备用"
	if empty.Valid {
		emptyOrFallback = ""
	}
	for _, tc := range []struct {
		name        string
		first, next any
		want        sql.NullString
	}{
		{"first_value", "中文", "备用", sql.NullString{String: "中文", Valid: true}},
		{"fallback", nil, "默认值", sql.NullString{String: "默认值", Valid: true}},
		{"both_null", nil, nil, sql.NullString{}},
		{"empty_string", "", "备用", sql.NullString{String: emptyOrFallback, Valid: true}},
		{"full_length", nil, fullLength, sql.NullString{String: fullLength, Valid: true}},
		{"excess_trailing_space", trailingSpaceBoundary + " ", nil, sql.NullString{String: trailingSpaceBoundary, Valid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got sql.NullString
			require.NoError(t, db.QueryRowContext(ctx, query, tc.first, tc.next).Scan(&got))
			require.Equal(t, tc.want, got)
		})
	}
	var value sql.NullString
	err := db.QueryRowContext(ctx, query, fullLength+"字", nil).Scan(&value)
	require.Equal(t, "22001", compatibilitySQLState(err), "first argument length error: %v", err)
	err = db.QueryRowContext(ctx, query, "first", fullLength+"字").Scan(&value)
	require.Equal(t, "22001", compatibilitySQLState(err), "second argument length error: %v", err)
}

func TestCompatibilitySessionFunctionsPreserveExistingSQLIntegration(t *testing.T) {
	db := openUUIDIntegrationDB(t, "DBGOLD_TEST_POSTGRES_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := newBusinessIntegrationSchema(t, ctx, db)
	getdate := qualifiedFunction(schema, "getdate")
	getrowid := qualifiedFunction(schema, "getrowid")
	businessExec(t, ctx, db, "CREATE FUNCTION "+getdate+"() RETURNS timestamp LANGUAGE sql AS $$SELECT timestamp '2001-02-03 04:05:06'$$")
	businessExec(t, ctx, db, "CREATE FUNCTION "+getrowid+"() RETURNS SETOF integer LANGUAGE sql AS $$SELECT 99$$")

	w := builtinIntegrationWriter(db, schema, "postgres")
	report, err := EnsureCompatibilityFunctions(ctx, w, CompatibilityRoutineNames{Functions: []string{"getdate", "getrowid", "isnull"}})
	require.ErrorContains(t, err, "getrowid")
	require.Equal(t, 1, report.Failed)
	statuses := make(map[string]string)
	for _, item := range report.Items {
		statuses[item.Name] = item.Status
	}
	require.Equal(t, "reused", statuses["getdate"])
	require.Equal(t, "failed", statuses["getrowid"])
	require.Equal(t, "created", statuses["isnull"])
	var unchanged bool
	query := "SELECT " + getdate + "() = timestamp '2001-02-03 04:05:06' AND existing.id = 99 FROM " + getrowid + "() AS existing(id)"
	require.NoError(t, db.QueryRowContext(ctx, query).Scan(&unchanged))
	require.True(t, unchanged)
}
