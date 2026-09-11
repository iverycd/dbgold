package tsqldameng

import (
	"github.com/stretchr/testify/require"
	"math"
	"testing"
)

func TestDefaults(t *testing.T) {
	for raw, want := range map[string]string{
		"((0))": " DEFAULT 0", "((-123.40))": " DEFAULT -123.40", "(N'中文 O''Brien')": " DEFAULT '中文 O''Brien'", "(n'(')": " DEFAULT '('", "('')": " DEFAULT ''", "('NULL')": " DEFAULT 'NULL'", "(NULL)": " DEFAULT NULL", "('getdate()')": " DEFAULT 'getdate()'", "(getdate())": " DEFAULT LOCALTIMESTAMP", "(SYSDATETIME())": " DEFAULT LOCALTIMESTAMP", "('a\\b')": " DEFAULT 'a\\b'",
	} {
		got, e := Default(raw)
		require.NoError(t, e, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"newid()", "newsequentialid()", "1+2", "('x')+'y'", "N'bad", "((1)", "(SELECT 1)"} {
		_, e := Default(raw)
		require.Error(t, e, raw)
	}
}
func TestViewsPreserveValuesAndScope(t *testing.T) {
	got, deps, e := View(`CREATE VIEW [dbo].[V] AS SELECT [t].[Mixed] AS [Alias], N'AbC + TOP 10 dbo.X O''Brien\n' AS [Text] FROM [dbo].[Ta]]ble] t WHERE t.[Mixed] = N'YES'`, "Target")
	require.NoError(t, e)
	require.Equal(t, []string{"Ta]ble"}, deps)
	require.Contains(t, got, `"TARGET"."TA]BLE" "T"`)
	require.Contains(t, got, `"T"."MIXED" AS "ALIAS"`)
	require.Contains(t, got, `'AbC + TOP 10 dbo.X O''Brien\n'`)
	require.Contains(t, got, `'YES'`)
	_, _, e = View(`CREATE VIEW dbo.V AS SELECT a.Id, COUNT(*) AS Total FROM dbo.A a LEFT JOIN dbo.B b ON a.Id=b.AId GROUP BY a.Id`, "APP")
	require.NoError(t, e)
}
func TestUnsupportedViews(t *testing.T) {
	for _, body := range []string{"SELECT TOP 1 Id FROM dbo.T", "SELECT a + b FROM dbo.T", "SELECT x FROM other.T", "SELECT GETDATE() AS Now", "SELECT x FROM dbo.T ORDER BY x", "SELECT CAST(x AS varchar(10)) FROM dbo.T", "SELECT x FROM db.dbo.T", "SELECT x FROM dbo.T; DELETE FROM dbo.T", "SELECT ROW_NUMBER() OVER(ORDER BY x) FROM dbo.T", "SELECT x COLLATE abc FROM dbo.T"} {
		_, _, e := View("CREATE VIEW dbo.V AS "+body, "APP")
		require.Error(t, e, body)
	}
	_, _, e := View("", "APP")
	require.Error(t, e)
}
func TestIdentityNext(t *testing.T) {
	ptr := func(n int64) *int64 { return &n }
	for _, tc := range []struct {
		seed, step   int64
		last, lo, hi *int64
		want         int64
	}{
		{10, 5, nil, nil, nil, 10}, {10, 5, ptr(50), ptr(10), ptr(40), 55}, {10, 5, ptr(50), ptr(10), ptr(57), 60},
		{-10, -5, ptr(-30), ptr(-40), ptr(-10), -45}, {1, 1, ptr(100), nil, nil, 101},
	} {
		n, e := IdentityNext(tc.seed, tc.step, tc.last, tc.lo, tc.hi)
		require.NoError(t, e)
		require.Equal(t, tc.want, n)
	}
	_, e := IdentityNext(1, 1, ptr(math.MaxInt64), nil, nil)
	require.Error(t, e)
	_, e = IdentityNext(-1, -1, ptr(math.MinInt64), nil, nil)
	require.Error(t, e)
}
