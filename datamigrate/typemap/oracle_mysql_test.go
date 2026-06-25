package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/assert"
)

func TestOracleToMySQL(t *testing.T) {
	cases := []struct {
		name     string
		col      source.ColumnInfo
		expected string
	}{
		// NUMBER 按 precision 分级(不依赖 AVG_COL_LEN)
		{"number_p4_smallint", source.ColumnInfo{DataType: "NUMBER", Precision: 4, Scale: 0}, "smallint"},
		{"number_p9_int", source.ColumnInfo{DataType: "NUMBER", Precision: 9, Scale: 0}, "int"},
		{"number_p18_bigint", source.ColumnInfo{DataType: "NUMBER", Precision: 18, Scale: 0}, "bigint"},
		{"number_p38_decimal", source.ColumnInfo{DataType: "NUMBER", Precision: 38, Scale: 0}, "decimal(38,0)"},
		{"number_decimal", source.ColumnInfo{DataType: "NUMBER", Precision: 10, Scale: 2}, "decimal(10,2)"},
		{"number_no_prec_double", source.ColumnInfo{DataType: "NUMBER"}, "double"},
		{"number_prec_overflow", source.ColumnInfo{DataType: "NUMBER", Precision: 70, Scale: 40}, "decimal(65,30)"},

		// 浮点
		{"float", source.ColumnInfo{DataType: "FLOAT", Precision: 126}, "double"},
		{"binary_double", source.ColumnInfo{DataType: "BINARY_DOUBLE"}, "double"},
		{"binary_float", source.ColumnInfo{DataType: "BINARY_FLOAT"}, "float"},

		// 字符:正常 + 超长降级
		{"varchar2_normal", source.ColumnInfo{DataType: "VARCHAR2", Length: 255}, "varchar(255)"},
		{"varchar2_overflow_text", source.ColumnInfo{DataType: "VARCHAR2", Length: 20000}, "text"},
		{"nvarchar2", source.ColumnInfo{DataType: "NVARCHAR2", Length: 100}, "varchar(100)"},
		{"char_normal", source.ColumnInfo{DataType: "CHAR", Length: 10}, "char(10)"},
		{"char_overflow_varchar", source.ColumnInfo{DataType: "CHAR", Length: 500}, "varchar(500)"},
		{"nchar", source.ColumnInfo{DataType: "NCHAR", Length: 20}, "char(20)"},

		// 大对象
		{"clob", source.ColumnInfo{DataType: "CLOB"}, "longtext"},
		{"nclob", source.ColumnInfo{DataType: "NCLOB"}, "longtext"},
		{"long", source.ColumnInfo{DataType: "LONG"}, "longtext"},
		{"blob", source.ColumnInfo{DataType: "BLOB"}, "longblob"},
		{"raw", source.ColumnInfo{DataType: "RAW", Length: 16}, "longblob"},
		{"long_raw", source.ColumnInfo{DataType: "LONG RAW"}, "longblob"},

		// 日期时间
		{"date", source.ColumnInfo{DataType: "DATE"}, "datetime"},
		{"timestamp", source.ColumnInfo{DataType: "TIMESTAMP(6)"}, "datetime(6)"},
		{"timestamp_tz", source.ColumnInfo{DataType: "TIMESTAMP(6) WITH TIME ZONE"}, "datetime(6)"},
		{"timestamp_ltz", source.ColumnInfo{DataType: "TIMESTAMP WITH LOCAL TIME ZONE"}, "datetime(6)"},

		// 文档未覆盖、本实现补齐
		{"interval_ym", source.ColumnInfo{DataType: "INTERVAL YEAR(2) TO MONTH"}, "varchar(50)"},
		{"interval_ds", source.ColumnInfo{DataType: "INTERVAL DAY TO SECOND"}, "varchar(50)"},
		{"boolean", source.ColumnInfo{DataType: "BOOLEAN"}, "tinyint(1)"},
		{"xmltype", source.ColumnInfo{DataType: "XMLTYPE"}, "longtext"},
		{"rowid", source.ColumnInfo{DataType: "ROWID"}, "varchar(100)"},
		{"urowid", source.ColumnInfo{DataType: "UROWID", Length: 4000}, "varchar(4000)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.expected, OracleToMySQL(c.col, false, false))
		})
	}
}
