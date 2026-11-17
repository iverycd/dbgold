package typemap

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCrossOracleTypes(t *testing.T) {
	for _, tc := range []struct {
		src, typ, want string
		p, scale       int64
	}{
		{"sqlserver", "timestamp", "RAW(8)", 0, 0}, {"sqlserver", "rowversion", "RAW(8)", 0, 0}, {"sqlserver", "uniqueidentifier", "VARCHAR2(36 CHAR)", 0, 0}, {"sqlserver", "xml", "CLOB", 0, 0}, {"sqlserver", "time", "VARCHAR2(18 CHAR)", 0, 7}, {"sqlserver", "datetime2", "TIMESTAMP(7)", 0, 7}, {"sqlserver", "datetimeoffset", "TIMESTAMP(7) WITH TIME ZONE", 0, 7}, {"sqlserver", "decimal", "NUMBER(38,18)", 38, 18}, {"sqlserver", "float", "BINARY_DOUBLE", 0, 0}, {"dameng", "timestamp", "TIMESTAMP(9)", 0, 9}, {"dameng", "date", "DATE", 0, 0},
	} {
		t.Run(tc.src+tc.typ, func(t *testing.T) {
			got, e := OracleTypeForSource(source.ColumnInfo{Name: "Mixed", DataType: tc.typ, Precision: tc.p, Scale: tc.scale, DateTimePrecision: tc.scale}, tc.src, false, 4, 4)
			require.NoError(t, e)
			require.Equal(t, tc.want, got)
		})
	}
	for _, src := range []string{"dameng", "sqlserver"} {
		for _, typ := range []string{"geometry", "sql_variant", "custom"} {
			_, e := OracleTypeForSource(source.ColumnInfo{DataType: typ}, src, false, 4, 4)
			require.Error(t, e)
		}
	}
	for _, storage := range []string{"decimal", "real", "double"} {
		c := source.ColumnInfo{DataType: "FLOAT", DamengFloatStorage: storage}
		got, e := OracleTypeForSource(c, "dameng", false, 4, 4)
		require.NoError(t, e)
		require.Equal(t, map[string]string{"decimal": "NUMBER", "real": "BINARY_FLOAT", "double": "BINARY_DOUBLE"}[storage], got)
	}
}
func TestDamengSQLServerTypes(t *testing.T) {
	for typ, want := range map[string]string{"INTEGER": "INT", "BIGINT": "BIGINT", "DATE": "DATE", "BOOLEAN": "BIT", "CLOB": "NVARCHAR(MAX)", "BLOB": "VARBINARY(MAX)"} {
		got, e := SQLServerTypeForSource(source.ColumnInfo{DataType: typ}, "dameng")
		require.NoError(t, e)
		require.Equal(t, want, got)
	}
	_, e := SQLServerTypeForSource(source.ColumnInfo{DataType: "TIMESTAMP", DateTimePrecision: 9}, "dameng")
	require.Error(t, e, "never round DM nanoseconds to SQL Server's seven digits")
}
