package target

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// MySQL's reader delivers BIGINT UNSIGNED as uint64. Verify the destination
// bytes through a textual server-side projection so no client integer or
// float conversion can hide an overflow or loss of precision.
func TestMySQLDMBatchUnsignedIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_MYSQL_SCAN_DM_DSN")
	if dsn == "" {
		t.Skip("requires a disposable DM instance in DBGOLD_MYSQL_SCAN_DM_DSN")
	}
	admin, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer admin.Close()
	schema := fmt.Sprintf("DM_U64_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	defer func() {
		_, err := admin.ExecContext(t.Context(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
	}()
	values := []uint64{0, math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64 - 1, math.MaxUint64}
	rows := make([][]any, len(values))
	want := make([]string, len(values))
	for i, value := range values {
		rows[i] = []any{int64(i + 1), value}
		want[i] = strconv.FormatUint(value, 10)
	}
	for _, strategy := range []string{"ROW", "BATCH"} {
		t.Run(strategy, func(t *testing.T) {
			w, err := NewDaMeng(dsn, schema, ConnPoolConfig{MaxOpenConns: 1})
			require.NoError(t, err)
			defer w.Close()
			w.SetSourceType("mysql")
			write := w.CopyData
			if strategy == "BATCH" {
				write = w.CopyMySQLBatchData
			}
			cols, types := []string{"K", "V"}, []string{"BIGINT", "UNSIGNED BIGINT"}
			exact, narrow := "U64_EXACT_"+strategy, "U64_NARROW_"+strategy
			_, err = admin.ExecContext(t.Context(), "CREATE TABLE "+schema+"."+exact+" (K BIGINT PRIMARY KEY,V NUMBER(20,0) NOT NULL)")
			require.NoError(t, err)
			require.NoError(t, write(t.Context(), exact, cols, types, rows))
			result, err := admin.QueryContext(t.Context(), "SELECT CAST(V AS VARCHAR(128)) FROM "+schema+"."+exact+" ORDER BY K")
			require.NoError(t, err)
			defer result.Close()
			var got []string
			for result.Next() {
				var value string
				require.NoError(t, result.Scan(&value))
				got = append(got, value)
			}
			require.NoError(t, result.Err())
			require.Equal(t, want, got)
			require.NoError(t, result.Close())

			_, err = admin.ExecContext(t.Context(), "CREATE TABLE "+schema+"."+narrow+" (K BIGINT PRIMARY KEY,V BIGINT NOT NULL)")
			require.NoError(t, err)
			require.NoError(t, write(t.Context(), narrow, cols, types, [][]any{{int64(-1), uint64(42)}}))
			// Prefix rows fit BIGINT. The first >MaxInt64 value must reject the
			// whole second page, even if a native chunk already wrote its prefix.
			require.Error(t, write(t.Context(), narrow, cols, types, rows))
			var n, key, value int64
			require.NoError(t, admin.QueryRowContext(t.Context(), "SELECT COUNT(*),MIN(K),MIN(V) FROM "+schema+"."+narrow).Scan(&n, &key, &value))
			require.Equal(t, []int64{1, -1, 42}, []int64{n, key, value})
		})
	}
}
