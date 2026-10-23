package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengDocumentedTypes(t *testing.T) {
	for _, tc := range []struct {
		c    source.ColumnInfo
		want string
	}{
		{source.ColumnInfo{DataType: "DATE"}, "TIMESTAMP(0)"},
		{source.ColumnInfo{DataType: "TIMESTAMP(9) WITH LOCAL TIME ZONE"}, "TIMESTAMP(9) WITH TIME ZONE"},
		{source.ColumnInfo{DataType: "NUMBER", Precision: 38, Scale: 19}, "NUMBER(38,19)"},
		{source.ColumnInfo{DataType: "FLOAT", Precision: 126}, "FLOAT(126)"},
		{source.ColumnInfo{DataType: "REAL", Precision: 63}, "FLOAT(63)"},
		{source.ColumnInfo{DataType: "DOUBLE PRECISION"}, "FLOAT"},
		{source.ColumnInfo{DataType: "FLOAT(24)"}, "FLOAT(24)"},
		{source.ColumnInfo{DataType: "FLOAT", Precision: 127}, "FLOAT"},
		{source.ColumnInfo{DataType: "BINARY_FLOAT"}, "REAL"},
		{source.ColumnInfo{DataType: "BINARY_DOUBLE"}, "DOUBLE"},
		{source.ColumnInfo{DataType: "INTEGER"}, "NUMBER(38,0)"},
		{source.ColumnInfo{DataType: "VARCHAR2", Length: 10, DataLength: 40, CharUsed: "C"}, "VARCHAR2(10 CHAR)"},
		{source.ColumnInfo{DataType: "VARCHAR2", Length: 40, DataLength: 40, CharUsed: "B"}, "VARCHAR2(40)"},
		{source.ColumnInfo{DataType: "RAW", DataLength: 2000}, "VARBINARY(2000)"},
		{source.ColumnInfo{DataType: "LONG"}, "TEXT"},
		{source.ColumnInfo{DataType: "LONG RAW"}, "LONGVARBINARY"},
		{source.ColumnInfo{DataType: "NCLOB"}, "CLOB"},
		{source.ColumnInfo{DataType: "BLOB"}, "BLOB"},
		{source.ColumnInfo{DataType: "BFILE"}, "BFILE"},
		{source.ColumnInfo{DataType: "XMLTYPE"}, "XMLTYPE"},
		{source.ColumnInfo{DataType: "ROWID", DataLength: 10}, "BINARY(18)"},
		{source.ColumnInfo{DataType: "UROWID", DataLength: 4000}, "VARBINARY(4000)"},
		{source.ColumnInfo{DataType: "XMLTYPE", TypeKind: "user_defined", TypeSchema: "SYS"}, "XMLTYPE"},
		{source.ColumnInfo{DataType: "SDO_GEOMETRY", TypeKind: "user_defined", TypeSchema: "MDSYS"}, "SYSGEO2.ST_GEOMETRY"},
		{source.ColumnInfo{DataType: "ST_GEOMETRY", TypeKind: "user_defined", TypeSchema: "SDE"}, "SYSGEO2.ST_GEOMETRY"},
		{source.ColumnInfo{DataType: "ST_POINT", TypeKind: "user_defined", TypeSchema: "SYSGEO"}, "SYSGEO2.ST_POINT"},
		{source.ColumnInfo{DataType: "INTERVAL DAY(9) TO SECOND(6)"}, "INTERVAL DAY(9) TO SECOND(6)"},
	} {
		require.NoError(t, ValidateOracleDamengColumn(tc.c))
		require.Equal(t, tc.want, OracleToDameng(tc.c, false, false))
	}
	mapper, ok := Get("oracle", "dameng")
	require.True(t, ok)
	require.Equal(t, "TIMESTAMP(0)", mapper(source.ColumnInfo{DataType: "DATE"}, false, false))
}

func TestOracleDamengRejectsLossyColumns(t *testing.T) {
	for _, c := range []source.ColumnInfo{
		{DataType: "NUMBER", Precision: 5, Scale: -2}, {DataType: "NUMBER", Precision: 4, Scale: 5},
		{DataType: "INTERVAL DAY(2) TO SECOND(9)"}, {DataType: "UNSUPPORTED_TYPE"},
		{DataType: "XMLTYPE", TypeKind: "user_defined", TypeSchema: "APP"},
		{DataType: "XMLTYPE", TypeKind: "user_defined", TypeSchema: "sys"},
		{DataType: "ST_GEOMETRY", TypeKind: "user_defined", TypeSchema: "APP"},
		{DataType: "ST_POINT; DROP TABLE T", TypeKind: "user_defined", TypeSchema: "SYSGEO"},
		{DataType: "XMLTYPE", TypeKind: "domain", TypeSchema: "SYS"},
		{DataType: "NUMBER", Identity: "ALWAYS"}, {DataType: "NUMBER", Identity: "BY DEFAULT"},
		{DataType: "NUMBER", Generated: "VIRTUAL"},
	} {
		require.Error(t, ValidateOracleDamengColumn(c))
	}
	require.NoError(t, ValidateOracleDamengColumn(source.ColumnInfo{DataType: "NUMBER", Identity: "BY DEFAULT ON NULL"}))
}

func TestOracleDamengCharacterUnitsAndExplicitNationalOverride(t *testing.T) {
	for _, tc := range []struct {
		kind, want string
	}{
		{"VARCHAR", "VARCHAR2"}, {"VARCHAR2", "VARCHAR2"},
		{"CHAR VARYING", "VARCHAR2"}, {"CHARACTER VARYING", "VARCHAR2"},
		{"CHAR", "CHAR"}, {"CHARACTER", "CHAR"},
		{"NCHAR", "NCHAR"}, {"NATIONAL CHAR", "NCHAR"}, {"NATIONAL CHARACTER", "NCHAR"},
		{"NVARCHAR2", "NVARCHAR"}, {"NCHAR VARYING", "NVARCHAR"},
		{"NATIONAL CHAR VARYING", "NVARCHAR"}, {"NATIONAL CHARACTER VARYING", "NVARCHAR"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c := source.ColumnInfo{DataType: tc.kind, Length: 10, DataLength: 40, CharUsed: "C"}
			require.NoError(t, ValidateOracleDamengColumn(c))
			require.Equal(t, tc.want+"(10 CHAR)", OracleToDameng(c, false, false))
			require.Equal(t, "NVARCHAR(10 CHAR)", OracleToDameng(c, false, true))
			c.CharUsed = "B"
			require.Equal(t, tc.want+"(10 CHAR)", OracleToDameng(c, true, false))
			if tc.want == "CHAR" || tc.want == "VARCHAR2" {
				require.Equal(t, tc.want+"(40)", OracleToDameng(c, false, false))
			}
		})
	}
}

func TestOracleDamengBaseTypeRecognizesAnnotatedNumericTypes(t *testing.T) {
	for raw, want := range map[string]string{
		"NUMBER(38,0)": "NUMBER", "DECIMAL(20, -2)": "DECIMAL",
		"FLOAT( 126 )": "FLOAT", "TIMESTAMP(9) WITH TIME ZONE": "TIMESTAMP WITH TIME ZONE",
		"INTERVAL DAY(9) TO SECOND(6)": "INTERVAL DAY TO SECOND",
	} {
		require.Equal(t, want, OracleDamengBaseType(raw))
	}
	require.Equal(t, "FLOAT(126)", OracleToDameng(source.ColumnInfo{DataType: "FLOAT( 126 )"}, false, false))
	require.Equal(t, "FLOAT", OracleToDameng(source.ColumnInfo{DataType: "FLOAT(24,2)"}, false, false))
}
