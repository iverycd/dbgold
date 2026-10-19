package oracledameng

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOracleDamengDefaultSubset(t *testing.T) {
	for raw, want := range map[string]string{"'SYSDATE'": " DEFAULT 'SYSDATE'", "((42.5))": " DEFAULT 42.5", "'O''Brien'": " DEFAULT 'O''Brien'", "SYSDATE": " DEFAULT LOCALTIMESTAMP(0)", "CURRENT_DATE": " DEFAULT LOCALTIMESTAMP(0)", "SYSTIMESTAMP": " DEFAULT CURRENT_TIMESTAMP", "DATE '2024-01-02'": " DEFAULT DATE '2024-01-02'", "TO_DATE('2024-01-02 03:04:05', 'YYYY-MM-DD HH24:MI:SS')": " DEFAULT TO_TIMESTAMP('2024-01-02 03:04:05', 'YYYY-MM-DD HH24:MI:SS')"} {
		got, e := Default(raw)
		require.NoError(t, e)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"SEQ.NEXTVAL", "SYS_GUID()", "my_function()", "42; DROP TABLE T", "'bad' || 'sql'", "TO_DATE('x', unsafe())", "('unterminated)"} {
		_, e := Default(raw)
		require.Error(t, e)
	}
}
