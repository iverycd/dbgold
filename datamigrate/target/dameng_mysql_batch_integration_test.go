package target

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// With one pooled writer connection, the next INSERT exercises the same
// physical session, or a replacement if the driver discarded it on cancel.
func TestMySQLDMBatchIdentityCleanupIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_MYSQL_SCAN_DM_DSN")
	if dsn == "" {
		t.Skip("requires a disposable DM instance in DBGOLD_MYSQL_SCAN_DM_DSN")
	}
	admin, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer admin.Close()
	ns := fmt.Sprintf("DM_IDCHECK_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE SCHEMA " + ns + " AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	defer func() {
		_, err := admin.Exec("DROP SCHEMA " + ns + " CASCADE")
		require.NoError(t, err)
	}()
	name := ns + ".IDCHECK"
	_, err = admin.Exec("CREATE TABLE " + name + " (ID BIGINT IDENTITY(1,1) PRIMARY KEY, V INT UNIQUE CHECK(V>0))")
	require.NoError(t, err)
	w, err := NewDaMeng(dsn, ns, ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	require.NoError(t, err)
	defer w.Close()
	w.SetSourceType("mysql")
	copyRows := func(ctx context.Context, rows [][]any) error {
		return w.CopyMySQLBatchData(ctx, "IDCHECK", []string{"ID", "V"}, []string{"BIGINT", "INT"}, rows)
	}
	identityOff := func() {
		t.Helper()
		_, err := w.db.Exec("INSERT INTO " + name + "(ID,V) VALUES (999,999)")
		require.Error(t, err, "explicit identity insert on the returned session must be rejected")
		_, err = w.db.Exec("INSERT INTO " + name + "(V) VALUES (777)")
		require.NoError(t, err, "the returned connection must be usable")
		_, err = w.db.Exec("DELETE FROM " + name + " WHERE V=777")
		require.NoError(t, err)
	}
	require.NoError(t, copyRows(t.Context(), [][]any{{int64(100), int64(10)}, {int64(101), int64(11)}}))
	require.Error(t, copyRows(t.Context(), [][]any{{int64(200), int64(20)}, {int64(201), int64(-1)}}))
	identityOff()

	// An uncommitted unique value keeps the array execution waiting in DM.
	blocker, err := admin.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec("INSERT INTO " + name + "(V) VALUES (42)")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	before, _ := w.BatchWriteStats()
	done := make(chan error, 1)
	go func() { done <- copyRows(ctx, [][]any{{int64(400), int64(41)}, {int64(401), int64(42)}}) }()
	require.Eventually(t, func() bool { n, _ := w.BatchWriteStats(); return n > before }, 5*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, blocker.Rollback())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled DM writer failed to release its transaction")
	}
	identityOff()
	var rows int
	require.NoError(t, admin.QueryRow("SELECT COUNT(*) FROM "+name).Scan(&rows))
	require.Equal(t, 2, rows, "only the previously committed page should remain")
}
