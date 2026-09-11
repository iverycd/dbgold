package typemap

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSQLServerTypes(t *testing.T) {
	for _, tt := range []struct {
		typ, formatted string
		length, p, s   int64
		want           string
	}{{"tinyint", "tinyint", 0, 3, 0, "SMALLINT"}, {"tinyint", "tinyint unsigned", 0, 3, 0, "TINYINT"}, {"int", "int unsigned", 0, 10, 0, "BIGINT"}, {"bigint", "bigint unsigned", 0, 20, 0, "DECIMAL(20,0)"}, {"varchar", "", 2000, 0, 0, "NVARCHAR(4000)"}, {"varchar", "", 2001, 0, 0, "NVARCHAR(MAX)"}, {"decimal", "", 0, 38, 8, "DECIMAL(38,8)"}, {"datetime", "", 0, 0, 0, "DATETIME2(0)"}, {"json", "", 0, 0, 0, ""}, {"enum", "", 10, 0, 0, ""}, {"geometry", "", 0, 0, 0, ""}, {"decimal", "", 0, 39, 0, ""}} {
		got, e := SQLServerType(source.ColumnInfo{Name: "C", DataType: tt.typ, FormattedType: tt.formatted, Length: tt.length, Precision: tt.p, Scale: tt.s})
		require.Equal(t, tt.want, got)
		if tt.want == "" {
			require.Error(t, e)
		} else {
			require.NoError(t, e)
		}
	}
	_, ok := Get("mysql", "sqlserver")
	require.True(t, ok)
	_, ok = Get("oracle", "sqlserver")
	require.False(t, ok)
}
