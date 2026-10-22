package source

import (
	"context"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

// This gate is observational: it never creates fixtures, advances NEXTVAL,
// changes system parameters or executes source DDL/DML. Point it at a prepared
// schema containing the native object variants being accepted for deployment.
func TestDaMengNativeReadOnlyCatalogIntegration(t *testing.T) {
	dsn, owner := os.Getenv("DM_NATIVE_TEST_SOURCE_DSN"), os.Getenv("DM_NATIVE_TEST_SOURCE_SCHEMA")
	if os.Getenv("DM_NATIVE_TEST_READONLY") != "1" || dsn == "" || owner == "" {
		t.Skip("需要 DM_NATIVE_TEST_READONLY=1、DM_NATIVE_TEST_SOURCE_DSN、DM_NATIVE_TEST_SOURCE_SCHEMA；仅进行只读原生目录与游标验收")
	}
	r, err := NewDaMengNative(dsn, owner, ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	snapshot, err := r.SnapshotDamengNative(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.Profile.Version)
	require.NotEmpty(t, snapshot.Profile.Instance)
	require.False(t, damengnative.HasErrors(snapshot.Issues), "预制原生对象夹具必须全部有可回放的定义: %+v", snapshot.Issues)
	for _, table := range snapshot.Tables {
		scan, err := r.OpenDamengNativeScan(ctx, table)
		require.NoError(t, err)
		if scan.Next() {
			_, err = scan.Values()
			require.NoError(t, err)
		}
		require.NoError(t, scan.Err())
		require.NoError(t, scan.Close())
	}
}
