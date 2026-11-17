package typemap

import (
	"dbgold/datamigrate/source"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLServerCrossSourceTypes(t *testing.T) {
	for _, tc := range []struct {
		src  string
		col  source.ColumnInfo
		want string
	}{
		{"oracle", source.ColumnInfo{DataType: "DATE"}, "DATETIME2(0)"},
		{"oracle", source.ColumnInfo{DataType: "TIMESTAMP(9) WITH TIME ZONE", DateTimePrecision: 9}, "DATETIMEOFFSET(7)"},
		{"oracle", source.ColumnInfo{DataType: "RAW", DataLength: 16}, "VARBINARY(16)"},
		{"oracle", source.ColumnInfo{DataType: "NUMBER", Precision: 10, Scale: -2}, "DECIMAL(12,0)"},
		{"oracle", source.ColumnInfo{DataType: "NUMBER"}, ""},
		{"oracle", source.ColumnInfo{DataType: "NUMBER", SQLServerPrecision: 38, SQLServerScale: 13}, "DECIMAL(38,13)"},
		{"oracle", source.ColumnInfo{DataType: "FLOAT", Precision: 126, SQLServerPrecision: 38, SQLServerScale: 4}, "DECIMAL(38,4)"},
		{"postgres", source.ColumnInfo{DataType: "bool"}, "BIT"},
		{"postgres", source.ColumnInfo{DataType: "uuid"}, "UNIQUEIDENTIFIER"},
		{"postgres", source.ColumnInfo{DataType: "timestamptz", DateTimePrecision: 6}, "DATETIMEOFFSET(6)"},
		{"postgres", source.ColumnInfo{DataType: "numeric", Precision: 9, Scale: 11}, "DECIMAL(11,11)"},
		{"postgres", source.ColumnInfo{DataType: "numeric", Precision: 38, Scale: -1}, ""},
		{"postgres", source.ColumnInfo{DataType: "_int4"}, ""},
		{"postgres", source.ColumnInfo{DataType: "int4", TypeKind: "d"}, ""},
	} {
		mapped, err := SQLServerTypeForSource(tc.col, tc.src)
		if tc.want == "" {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.want, mapped)
		}
	}
	for _, src := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		m, ok := Get(src, "sqlserver")
		require.True(t, ok)
		require.Equal(t, "INT", m(source.ColumnInfo{DataType: "int4"}, false, false))
	}
}
