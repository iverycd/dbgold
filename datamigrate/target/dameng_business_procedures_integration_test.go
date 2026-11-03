package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

// Run only in an explicitly opted-in disposable schema. Reference procedures
// use the supplied SQL verbatim except for their qualified CREATE names.
func TestDamengBusinessProceduresIntegration(t *testing.T) {
	dsn, schema := os.Getenv("MYSQL_DM_TEST_TARGET_DSN"), os.Getenv("MYSQL_DM_TEST_SCHEMA")
	if os.Getenv("MYSQL_DM_FUNCTION_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("实库未验证：需要独立达梦 schema、MYSQL_DM_TEST_TARGET_DSN / MYSQL_DM_TEST_SCHEMA 和 MYSQL_DM_FUNCTION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	w, err := NewDaMeng(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	q := w.qualifiedTable
	refAlter, refNumber := "DBGOLD_REF_ALTER", "DBGOLD_REF_NUMBER"
	names := []string{"API_MANAGE_CONFIG", "CODE_MAXNUMBER", "UK_API_MAN_CON_APIGUID", refAlter, refNumber}
	for _, r := range dmBusinessRoutines() {
		names = append(names, r.name)
	}
	for _, name := range names {
		var n int
		require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?", w.schema, name).Scan(&n))
		require.Zero(t, n, "refuse to modify an existing fixture name: %s", name)
	}
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, r := range dmBusinessRoutines() {
			_, _ = w.db.ExecContext(clean, "DROP "+strings.ToUpper(r.kind)+" "+q(r.name))
		}
		for _, name := range []string{refAlter, refNumber} {
			_, _ = w.db.ExecContext(clean, "DROP PROCEDURE "+q(name))
		}
		for _, name := range []string{"API_MANAGE_CONFIG", "CODE_MAXNUMBER"} {
			_, _ = w.db.ExecContext(clean, "DROP TABLE "+q(name))
		}
	}()
	exec := func(statement string, args ...any) {
		t.Helper()
		_, err := w.db.ExecContext(ctx, statement, args...)
		require.NoError(t, err, statement)
	}
	exec("CREATE TABLE " + q("API_MANAGE_CONFIG") + "(APIGUID VARCHAR(50))")
	exec("CREATE TABLE " + q("CODE_MAXNUMBER") + "(NUMBERNAME NVARCHAR2(100),CURRENTVALUE VARCHAR(100),NUMBERFLAG NVARCHAR2(100))")
	exec("INSERT INTO " + q("CODE_MAXNUMBER") + " VALUES('sentinel','41','F')")
	for _, fn := range dmBusinessFunctions() {
		exec("CREATE FUNCTION " + q(fn.name) + " RETURN INT AS BEGIN RETURN 197; END;")
	}
	report, err := w.EnsureMySQLDamengBusinessFunctions(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 9, report.Total)
	require.Equal(t, 2, report.Created)
	require.Equal(t, 7, report.Reused)
	var count int
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_INDEXES WHERE OWNER=? AND INDEX_NAME='UK_API_MAN_CON_APIGUID'", w.schema).Scan(&count))
	require.Zero(t, count, "installation must not call ALTER procedure")
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("CODE_MAXNUMBER")+" WHERE NUMBERNAME='sentinel' AND CURRENTVALUE='41' AND NUMBERFLAG='F'").Scan(&count))
	require.Equal(t, 1, count)
	readSource := func(name string) string {
		rows, err := w.db.QueryContext(ctx, "SELECT TEXT FROM ALL_SOURCE WHERE OWNER=? AND NAME=? AND TYPE='PROCEDURE' ORDER BY LINE", w.schema, name)
		require.NoError(t, err)
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			b.WriteString(line)
		}
		require.NoError(t, rows.Err())
		require.NotEmpty(t, b.String())
		return b.String()
	}
	before := map[string]string{}
	for _, p := range dmBusinessProcedures() {
		before[p.name] = readSource(p.name)
	}
	report, err = w.EnsureMySQLDamengBusinessFunctions(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 9, report.Reused)
	require.Zero(t, report.Created)
	for _, p := range dmBusinessProcedures() {
		require.Equal(t, before[p.name], readSource(p.name))
	}
	exec(strings.Replace(dmEpointProcAlterOriginal, "CREATE OR REPLACE PROCEDURE EPOINT_PROC_ALTER", "CREATE PROCEDURE "+q(refAlter), 1))
	exec(strings.Replace(dmGetCurrentNumberOriginal, "CREATE OR REPLACE procedure GETCURRENTNUMBER", "CREATE PROCEDURE "+q(refNumber), 1))
	conn, err := w.db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	_, err = conn.ExecContext(ctx, "SET SCHEMA "+w.dia.QuoteIdent(w.schema))
	require.NoError(t, err)
	call := func(name string, args ...any) error {
		_, err := conn.ExecContext(ctx, "CALL "+q(name)+"("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+")", args...)
		return err
	}
	sameError := func(t *testing.T, want, got error) {
		t.Helper()
		if want == nil {
			require.NoError(t, got)
			return
		}
		require.Error(t, got)
		var a, b *dm.DmError
		require.True(t, errors.As(want, &a), "reference: %v", want)
		require.True(t, errors.As(got, &b), "installed: %v", got)
		require.Equal(t, a.ErrCode, b.ErrCode)
	}
	t.Run("unique_constraint", func(t *testing.T) {
		for _, name := range []string{refAlter, "EPOINT_PROC_ALTER"} {
			require.NoError(t, call(name))
			require.NoError(t, call(name), "repeat call must preserve existing index")
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME='API_MANAGE_CONFIG' AND CONSTRAINT_NAME='UK_API_MAN_CON_APIGUID' AND CONSTRAINT_TYPE='U'", w.schema).Scan(&count))
			require.Equal(t, 1, count)
			_, err := conn.ExecContext(ctx, "ALTER TABLE "+q("API_MANAGE_CONFIG")+" DROP CONSTRAINT UK_API_MAN_CON_APIGUID")
			require.NoError(t, err)
		}
	})
	type outcome struct {
		output sql.NullString
		values []sql.NullString
		err    error
	}
	run := func(t *testing.T, proc string, name, flag, length any, seed []any) []outcome {
		t.Helper()
		_, err := conn.ExecContext(ctx, "DELETE FROM "+q("CODE_MAXNUMBER"))
		require.NoError(t, err)
		for _, value := range seed {
			_, err = conn.ExecContext(ctx, "INSERT INTO "+q("CODE_MAXNUMBER")+" VALUES(?,?,?)", name, value, flag)
			require.NoError(t, err)
		}
		var results []outcome
		for i := 0; i < 2; i++ {
			result := outcome{}
			result.err = call(proc, name, flag, length, sql.Out{Dest: &result.output})
			rows, err := conn.QueryContext(ctx, "SELECT CURRENTVALUE FROM "+q("CODE_MAXNUMBER")+" ORDER BY CURRENTVALUE")
			require.NoError(t, err)
			for rows.Next() {
				var value sql.NullString
				require.NoError(t, rows.Scan(&value))
				result.values = append(result.values, value)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			results = append(results, result)
		}
		return results
	}
	for _, tc := range []struct {
		name                     string
		numberName, flag, length any
		seed                     []any
	}{
		{"new", "订单", "单", 4, nil}, {"existing", "order", "F", 4, []any{"41"}},
		{"null_name", nil, "F", 4, nil}, {"null_flag", "order", nil, 4, nil}, {"empty", "", "", 4, nil},
		{"zero", "order", "F", 4, []any{"0"}}, {"null_value", "order", "F", 4, []any{nil}},
		{"non_numeric", "order", "F", 4, []any{"bad"}}, {"multiple", "order", "F", 4, []any{"2", "9"}},
		{"null_length", "order", "F", nil, nil}, {"zero_length", "order", "F", 0, nil},
		{"at_length", "order", "F", 49, nil}, {"over_length", "order", "F", 50, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := run(t, refNumber, tc.numberName, tc.flag, tc.length, tc.seed)
			got := run(t, "GETCURRENTNUMBER", tc.numberName, tc.flag, tc.length, tc.seed)
			for i := range want {
				sameError(t, want[i].err, got[i].err)
				if want[i].err == nil {
					require.Equal(t, want[i].output, got[i].output)
				}
				require.Equal(t, want[i].values, got[i].values, "committed state must match even when output fails")
			}
			if tc.name == "new" {
				require.Equal(t, "单0001", got[0].output.String)
				require.Equal(t, "单0002", got[1].output.String)
			}
			if tc.name == "existing" {
				require.Equal(t, "F0042", got[0].output.String)
				require.Equal(t, "F0043", got[1].output.String)
			}
		})
	}
	t.Log(fmt.Sprintf("validated 2 procedures in %s", w.schema))
}
