package oracledameng

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOracleDamengDefaultSubset(t *testing.T) {
	for raw, want := range map[string]string{"'SYSDATE'": " DEFAULT 'SYSDATE'", "((42.5))": " DEFAULT 42.5", "'O''Brien'": " DEFAULT 'O''Brien'", "SYSDATE": " DEFAULT LOCALTIMESTAMP(0)", "CURRENT_DATE": " DEFAULT LOCALTIMESTAMP(0)", "SYSTIMESTAMP": " DEFAULT CURRENT_TIMESTAMP", "DATE '2024-01-02'": " DEFAULT CAST(DATE '2024-01-02' AS TIMESTAMP(0))", "TO_DATE('2024-01-02 03:04:05', 'YYYY-MM-DD HH24:MI:SS')": " DEFAULT CAST(TO_TIMESTAMP('2024-01-02 03:04:05', 'YYYY-MM-DD HH24:MI:SS') AS TIMESTAMP(0))"} {
		got, e := Default(raw)
		require.NoError(t, e)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"SEQ.NEXTVAL", "SYS_GUID()", "my_function()", "42; DROP TABLE T", "'bad' || 'sql'", "TO_DATE('x', unsafe())", "('unterminated)"} {
		_, e := Default(raw)
		require.Error(t, e)
	}
}

func TestOracleDamengDefaultLiteralSemantics(t *testing.T) {
	for raw, want := range map[string]string{
		"''": " DEFAULT NULL", "N''": " DEFAULT NULL", "q'[]'": " DEFAULT NULL", "nq'[]'": " DEFAULT NULL",
		"q'[a,b]'": " DEFAULT 'a,b'", "(q'[a)b]')": " DEFAULT 'a)b'", "nq'[汉字'x]'": " DEFAULT N'汉字''x'",
		"TO_DATE('', 'YYYY-MM-DD')":  " DEFAULT CAST(TO_TIMESTAMP(NULL, 'YYYY-MM-DD') AS TIMESTAMP(0))",
		"CAST(N'x' AS NCHAR(10))":    " DEFAULT CAST(N'x' AS NCHAR(10 CHAR))",
		"CAST('x' AS NVARCHAR2(10))": " DEFAULT CAST('x' AS NVARCHAR(10 CHAR))",
	} {
		got, err := Default(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{`'C:\folder'`, `N'C:\folder'`, `q'[C:\folder]'`, `TO_DATE('x\y','YYYY')`} {
		_, err := Default(raw)
		require.ErrorContains(t, err, "反斜杠", raw)
	}
	for _, raw := range []string{"TO_DATE('x','y',)", "CAST('x' AS XMLTYPE)", "CAST(unsafe() AS VARCHAR2(20))", "CAST(1 AS NUMBER(0))", "CAST(TIMESTAMP '2024-01-02 03:04:05.9' AS DATE)"} {
		_, err := Default(raw)
		require.Error(t, err, raw)
	}
}

func TestOracleDamengDefaultOptions(t *testing.T) {
	got, err := DefaultWithOptions("DATE '2024-01-02'", Options{UseDate: true})
	require.NoError(t, err)
	require.Equal(t, " DEFAULT DATE '2024-01-02'", got)
	got, err = DefaultWithOptions("CAST('x' AS CHAR(10))", Options{UseNvarchar2: true})
	require.NoError(t, err)
	require.Equal(t, " DEFAULT CAST('x' AS NVARCHAR(10 CHAR))", got)
	got, err = DefaultWithOptions("-1e125", Options{Float: true})
	require.NoError(t, err)
	require.Equal(t, " DEFAULT -1e125", got)
	_, err = Default("-1e125")
	require.Error(t, err) // NUMBER retains its exact range guard.
	_, err = DefaultWithOptions("1e309", Options{Float: true})
	require.Error(t, err)
}
