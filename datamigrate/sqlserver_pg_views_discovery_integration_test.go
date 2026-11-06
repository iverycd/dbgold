package datamigrate

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

// Explicitly opt in with DBGOLD_SQLSERVER_PG_DISCOVERY=1 and
// DBGOLD_SQLSERVER_PG_SOURCE_DSN. This test reads only SQL Server catalogs and
// SERVERPROPERTY; it never opens a destination or executes generated view SQL.
// Optional DBGOLD_SQLSERVER_PG_DISCOVERY_EXPECTED_VIEWS checks inventory size.
// DBGOLD_SQLSERVER_PG_DISCOVERY_REQUIRED_VIEWS may override the comma-separated
// required fixes when using another source; the defaults cover the reported pair.
// If DBGOLD_SQLSERVER_PG_DISCOVERY_ALLOWED_DIAGNOSTICS is set, only those
// comma-separated view names may need manual adaptation; an empty value permits
// no diagnostics. Required views must always prepare successfully.
func TestSQLServerPGViewsDiscoveryIntegration(t *testing.T) {
	if os.Getenv("DBGOLD_SQLSERVER_PG_DISCOVERY") != "1" {
		t.Skip("需要 DBGOLD_SQLSERVER_PG_DISCOVERY=1 显式启用源库只读视图检查")
	}
	dsn := os.Getenv("DBGOLD_SQLSERVER_PG_SOURCE_DSN")
	if dsn == "" {
		t.Fatal("需要 DBGOLD_SQLSERVER_PG_SOURCE_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	versionDB, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal("无法初始化 SQL Server 元数据连接")
	}
	var version string
	err = versionDB.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))`).Scan(&version)
	versionDB.Close()
	if err != nil {
		// Connection-string parser errors can contain input fragments. Never
		// include the supplied DSN or a connection error in the test output.
		t.Fatal("无法读取 SQL Server 产品版本")
	}
	t.Logf("SQL Server product version: %s", version)
	reader, err := source.NewSQLServer(dsn, "", source.ConnPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	if err != nil {
		t.Fatal("无法连接 SQL Server 源库元数据")
	}
	t.Cleanup(func() { reader.Close() })
	views, err := reader.GetViewsForTarget(ctx, "kingbase")
	if err != nil {
		t.Fatal("无法读取 SQL Server 源库视图目录")
	}
	require.NotEmpty(t, views, "源库视图目录为空")
	if expectedText := os.Getenv("DBGOLD_SQLSERVER_PG_DISCOVERY_EXPECTED_VIEWS"); expectedText != "" {
		expected, parseErr := strconv.Atoi(expectedText)
		require.NoError(t, parseErr, "视图数量断言必须为整数")
		require.Positive(t, expected)
		require.Len(t, views, expected)
	}
	requiredText := os.Getenv("DBGOLD_SQLSERVER_PG_DISCOVERY_REQUIRED_VIEWS")
	if requiredText == "" {
		requiredText = "VIEW_FRAME_USER_SECONDOU,view_portal_myitem"
	}
	required := map[string]bool{}
	for _, name := range strings.Split(requiredText, ",") {
		if name = strings.TrimSpace(name); name != "" {
			required[strings.ToLower(name)] = false
		}
	}
	allowedText, strictDiagnostics := os.LookupEnv("DBGOLD_SQLSERVER_PG_DISCOVERY_ALLOWED_DIAGNOSTICS")
	allowedDiagnostics := map[string]bool{}
	for _, name := range strings.Split(allowedText, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowedDiagnostics[strings.ToLower(name)] = true
		}
	}
	m, _, writer := pgViewTestMigrator()
	m.reader = reader
	prepared, diagnostics := 0, 0
	for _, view := range views {
		key := strings.ToLower(view.ViewName)
		_, mustPrepare := required[key]
		if mustPrepare {
			required[key] = true
		}
		_, prepareErr := m.prepareView(ctx, view)
		if prepareErr != nil {
			diagnostics++
			message := strings.ReplaceAll(prepareErr.Error(), dsn, "[redacted]")
			t.Logf("view %s: diagnostic: %s", view.ViewName, message)
			if mustPrepare {
				t.Errorf("required view %s failed preparation: %s", view.ViewName, message)
			} else if strictDiagnostics && !allowedDiagnostics[key] {
				t.Errorf("unexpected diagnostic for view %s: %s", view.ViewName, message)
			}
			continue
		}
		prepared++
		t.Logf("view %s: prepared", view.ViewName)
	}
	for name, found := range required {
		if !found {
			t.Errorf("required view %s is absent from the source inventory", name)
		}
	}
	require.Empty(t, writer.views, "只读检查不得执行视图 DDL")
	t.Logf("views total=%d prepared=%d diagnostics=%d", len(views), prepared, diagnostics)
}
