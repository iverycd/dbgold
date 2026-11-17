package dialect

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCrossOracleDefaults(t *testing.T) {
	for _, tc := range []struct{ raw, typ, want string }{{"((42))", "NUMBER(10,0)", " DEFAULT 42"}, {"(N'MiXeD')", "NVARCHAR2(20)", " DEFAULT 'MiXeD'"}, {"SYSDATETIME()", "TIMESTAMP(7)", " DEFAULT LOCALTIMESTAMP(7)"}, {"NULL", "VARCHAR2(20 CHAR)", " DEFAULT NULL"}} {
		c := source.ColumnInfo{Name: "X", Default: &tc.raw}
		got, e := OracleDefaultForSource(c, "sqlserver", tc.typ)
		require.NoError(t, e)
		require.Equal(t, tc.want, got)
	}
	for _, raw := range []string{"NEWID()", "1; DROP TABLE T", "GETDATE()+1", "NEXT VALUE FOR unknown_seq"} {
		_, e := OracleDefaultForSource(source.ColumnInfo{Default: &raw}, "sqlserver", "NUMBER")
		require.Error(t, e)
	}
}
