package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// This optional fixture needs a third, prepared remote DM server. Its endpoint
// must be reachable from both DM servers, not merely from the Go test runner.
// It creates no remote objects and runs only a remote catalog count query.
func TestDamengNativeDBLinkIntegration(t *testing.T) {
	keys := []string{"SOURCE_DSN", "SOURCE_SCHEMA", "TARGET_DSN", "TARGET_SCHEMA", "REMOTE_CONNECT", "REMOTE_USERNAME", "REMOTE_PASSWORD"}
	values := map[string]string{}
	for _, key := range keys {
		values[key] = os.Getenv("DAMENG_NATIVE_LINK_" + key)
		if values[key] == "" {
			t.Skip("需要 DAMENG_NATIVE_LINK_SOURCE_DSN/SOURCE_SCHEMA/TARGET_DSN/TARGET_SCHEMA/REMOTE_CONNECT/REMOTE_USERNAME/REMOTE_PASSWORD 和 DAMENG_NATIVE_ALLOW_DDL=1；仅用于独立空测试 Schema")
		}
	}
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" {
		t.Skip("未启用专用 DBLink 实库 DDL 验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", values["SOURCE_DSN"])
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", values["TARGET_DSN"])
	require.NoError(t, err)
	defer dst.Close()
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, values["SOURCE_SCHEMA"]}, {dst, values["TARGET_SCHEMA"]}} {
		snapshot, err := damengnative.ReadCatalog(ctx, side.db, side.schema, false)
		require.NoError(t, err)
		require.Empty(t, snapshot.Objects, "DBLink 测试 Schema 必须完全为空")
	}
	const name = "NativeRemoteLink"
	cleanup := func(db *sql.DB, schema string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, "DROP LINK "+damengnative.Qualified(schema, name))
	}
	t.Cleanup(func() { cleanup(src, values["SOURCE_SCHEMA"]); cleanup(dst, values["TARGET_SCHEMA"]) })
	literal := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	ddl := "CREATE LINK " + damengnative.Qualified(values["SOURCE_SCHEMA"], name) + " CONNECT 'DPI' WITH " + damengnative.Quote(values["REMOTE_USERNAME"]) + " IDENTIFIED BY " + damengnative.Quote(values["REMOTE_PASSWORD"]) + " USING " + literal(values["REMOTE_CONNECT"])
	if _, err = src.ExecContext(ctx, ddl); err != nil {
		// Do not let an echoed source CREATE/password enter test diagnostics.
		t.Fatal("创建专用测试 DBLink 失败，服务端凭据详情已隐藏")
	}
	remoteCount := func(db *sql.DB, schema string) int64 {
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(schema))
		require.NoError(t, err)
		var count int64
		if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM SYS.SYSOBJECTS@"+damengnative.Quote(name)).Scan(&count); err != nil {
			t.Fatal("测试 DBLink 远端只读查询失败，服务端凭据详情已隐藏")
		}
		return count
	}
	want := remoteCount(src, values["SOURCE_SCHEMA"])
	require.Positive(t, want)
	r, err := source.NewDaMengNative(values["SOURCE_DSN"], values["SOURCE_SCHEMA"], source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(values["TARGET_DSN"], values["TARGET_SCHEMA"], target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	job := &Job{LogCh: make(chan string, 1024)}
	m := NewMigrator(r, w, job, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: values["TARGET_SCHEMA"], CaptureRepair: true})
	report := m.Run(ctx)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	for _, secret := range []string{values["REMOTE_PASSWORD"], values["REMOTE_CONNECT"]} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("DBLink 敏感连接信息泄漏到持久化报告")
		}
	}
	require.False(t, report.HasFailures(), "%+v", report.NativeSchema.Verification)
	require.Equal(t, want, remoteCount(dst, values["TARGET_SCHEMA"]), "目标必须通过复制的原生凭据连接同一远端")
}
