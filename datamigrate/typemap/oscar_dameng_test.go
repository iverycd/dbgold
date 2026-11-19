package typemap

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOscarDamengTypeMappings(t *testing.T) {
	mapper, ok := Get("oscar", "dameng")
	require.True(t, ok)
	for _, test := range []struct {
		types, want                       string
		length, precision, scale, seconds int64
	}{
		{"char|character|bpchar", "CHAR(3 CHAR)", 3, 0, 0, 0},
		{"character varying|varchar|nvarchar2", "VARCHAR(127 CHAR)", 127, 0, 0, 0},
		{`"char"`, "VARCHAR(1 CHAR)", 0, 0, 0, 0},
		{"name", "VARCHAR(127 CHAR)", 0, 0, 0, 0},
		{"text|array|integer[]", "TEXT", 0, 0, 0, 0},
		{"bit|binary", "BINARY(12)", 12, 0, 0, 0},
		{"bit varying|varbit|varbinary", "VARBINARY(12)", 12, 0, 0, 0},
		{"tinyint|int1", "TINYINT", 0, 0, 0, 0},
		{"smallint|int2", "SMALLINT", 0, 0, 0, 0},
		{"int|integer|int4|serial", "INT", 0, 0, 0, 0},
		{"bigint|int8|bigserial", "BIGINT", 0, 0, 0, 0},
		{"decimal|numeric|number", "NUMERIC(38,19)", 0, 38, 19, 0},
		{"numeric", "NUMERIC", 0, 0, 0, 0},
		{"real|float4", "FLOAT(24)", 0, 0, 0, 0},
		{"float|double precision|float8", "FLOAT(53)", 0, 0, 0, 0},
		{"date", "DATE", 0, 0, 0, 0},
		{"time", "TIME(6)", 0, 0, 0, 6},
		{"time with time zone|timetz", "TIME(4) WITH TIME ZONE", 0, 0, 0, 4},
		{"timestamp", "TIMESTAMP(9)", 0, 0, 0, 9},
		{"timestamp with time zone|timestamptz", "TIMESTAMP(6) WITH TIME ZONE", 0, 0, 0, 6},
		{"interval year to month", "INTERVAL YEAR(9) TO MONTH", 0, 0, 0, 0},
		{"interval day to second", "INTERVAL DAY(9) TO SECOND(6)", 0, 0, 6, 0},
		{"boolean|bool", "BIT", 0, 0, 0, 0},
		{"blob", "BLOB", 0, 0, 0, 0},
		{"clob", "CLOB", 0, 0, 0, 0},
		{"xmltype|xml", "XMLTYPE", 0, 0, 0, 0},
	} {
		for _, dt := range strings.Split(test.types, "|") {
			col := source.ColumnInfo{DataType: dt, Length: test.length, Precision: test.precision, Scale: test.scale, DateTimePrecision: test.seconds}
			require.Equal(t, test.want, mapper(col, true, true), dt)
		}
	}
}

func TestOscarDamengRejectsNarrowingAndUnknownTypes(t *testing.T) {
	for _, col := range []source.ColumnInfo{
		{DataType: "JSONB"},
		{DataType: "NUMERIC", Precision: 39, Scale: 19},
		{DataType: "TIME", DateTimePrecision: 7},
		{DataType: "INTERVAL DAY TO SECOND", Scale: 7},
		{DataType: "BIT", Length: 40_000},
		{DataType: "INT", Generated: "YES"},
		{DataType: "INT", Extra: "auto_increment"},
	} {
		_, err := OscarToDamengChecked(col)
		require.Error(t, err, col.DataType)
	}
}
