package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

// Compare with the supplied SQL in the same database/session instead of
// asserting a different transliteration algorithm or changing empty semantics.
func TestMySQLDamengFunGetPyIntegration(t *testing.T) {
	dsn, schema := os.Getenv("MYSQL_DM_TEST_TARGET_DSN"), os.Getenv("MYSQL_DM_TEST_SCHEMA")
	if os.Getenv("MYSQL_DM_FUNCTION_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("实库未验证：需要独立达梦测试 schema、MYSQL_DM_TEST_TARGET_DSN / MYSQL_DM_TEST_SCHEMA 和 MYSQL_DM_FUNCTION_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w, err := NewDaMeng(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	w.db.SetMaxOpenConns(1)
	referenceName := fmt.Sprintf("DBGOLD_PY_REF_%d", time.Now().UnixNano())
	for _, name := range []string{"FUN_GETPY", referenceName} {
		var n int
		require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=?", w.schema, name).Scan(&n))
		require.Zero(t, n, "test refuses to replace an existing object: %s", name)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, name := range []string{"FUN_GETPY", referenceName} {
			_, _ = w.db.ExecContext(cleanup, "DROP FUNCTION "+w.qualifiedTable(name))
		}
	}()
	original, err := os.ReadFile("testdata/dameng_fun_getpy_original.sql")
	require.NoError(t, err)
	referenceDDL := strings.Replace(string(original), "create or replace function fun_getpy", "create function "+w.qualifiedTable(referenceName), 1)
	require.NotEqual(t, string(original), referenceDDL)
	_, err = w.db.ExecContext(ctx, referenceDDL)
	require.NoError(t, err)
	var compiled string
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT STATUS FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='FUNCTION'", w.schema, referenceName).Scan(&compiled))
	require.Equal(t, "VALID", strings.TrimSpace(compiled))
	var spec dmBusinessFunction
	for _, candidate := range dmBusinessFunctions() {
		if candidate.name == "FUN_GETPY" {
			spec = candidate
			break
		}
	}
	require.Equal(t, "FUN_GETPY", spec.name)
	item := CompatibilityFunctionResult{Name: spec.name, DDL: w.dmBusinessFunctionDDL(spec)}
	require.NoError(t, w.ensureMySQLDamengBusinessFunction(ctx, spec, nil, &item))
	require.Equal(t, "created", item.Status)
	// Store the generated DDL for comparison after a second installation attempt.
	firstDDL := item.DDL
	require.NoError(t, w.ensureMySQLDamengBusinessFunction(ctx, spec, nil, &item))
	require.Equal(t, "reused", item.Status)
	require.Empty(t, item.DDL)
	require.Equal(t, "已存在，跳过，未校验定义", item.DefinitionWarning)
	require.Contains(t, firstDDL, dmFunGetPyBody)
	inputs := []any{nil, "", "中国", "张三A1", "重庆", "繁體", "ABC123😀", " 吖 ", "帀裠",
		"吖驁八簿嚓錯咑鵽妸樲发猤旮腂妎夻丌攈咔穒垃擽嘸椧拏瘧筽漚妑曝七裠亽鶸仨蜶侤籜屲鶩夕鑂丫韻帀咗",
		strings.Repeat("中", 2000), strings.Repeat("中", 2001), strings.Repeat("A", 2100)}
	for i, input := range inputs {
		t.Run(fmt.Sprintf("input_%d", i), func(t *testing.T) {
			var want, got sql.NullString
			refErr := w.db.QueryRowContext(ctx, "SELECT "+w.qualifiedTable(referenceName)+"(?) FROM DUAL", input).Scan(&want)
			gotErr := w.db.QueryRowContext(ctx, "SELECT "+w.qualifiedTable("FUN_GETPY")+"(?) FROM DUAL", input).Scan(&got)
			if refErr == nil {
				require.NoError(t, gotErr)
				require.Equal(t, want, got)
				return
			}
			require.Error(t, gotErr)
			var refDM, gotDM *dm.DmError
			require.True(t, errors.As(refErr, &refDM), "unexpected reference error: %v", refErr)
			require.True(t, errors.As(gotErr, &gotDM), "unexpected installed function error: %v", gotErr)
			require.Equal(t, refDM.ErrCode, gotDM.ErrCode, "original error: %v; installed error: %v", refErr, gotErr)
		})
	}
}
