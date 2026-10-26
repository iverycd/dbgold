package typemap

import (
	"fmt"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/assert"
)

func TestMySQLToDameng(t *testing.T) {
	cases := []struct {
		col          source.ColumnInfo
		charInLength bool
		useNvarchar2 bool
		expected     string
	}{
		{source.ColumnInfo{DataType: "tinyint"}, false, false, "TINYINT"},
		{source.ColumnInfo{DataType: "smallint"}, false, false, "SMALLINT"},
		{source.ColumnInfo{DataType: "mediumint"}, false, false, "INT"},
		{source.ColumnInfo{DataType: "int"}, false, false, "INT"},
		{source.ColumnInfo{DataType: "bigint"}, false, false, "BIGINT"},
		{source.ColumnInfo{DataType: "decimal", Precision: 10, Scale: 2}, false, false, "NUMBER(10,2)"},
		{source.ColumnInfo{DataType: "decimal", Precision: 65, Scale: 30}, false, false, "NUMBER(38,3)"},
		{source.ColumnInfo{DataType: "decimal", Precision: 40, Scale: 30}, false, false, "NUMBER(38,28)"},
		{source.ColumnInfo{DataType: "decimal", Precision: 50, Scale: 10}, false, false, "NUMBER(38,0)"},
		{source.ColumnInfo{DataType: "numeric", Precision: 65, Scale: 30}, false, false, "NUMBER(38,3)"},
		{source.ColumnInfo{DataType: "decimal"}, false, false, "NUMBER"},
		{source.ColumnInfo{DataType: "numeric"}, false, false, "NUMBER"},
		{source.ColumnInfo{DataType: "double"}, false, false, "DOUBLE"},
		{source.ColumnInfo{DataType: "float", Precision: 8, Scale: 2}, false, false, "REAL"},
		{source.ColumnInfo{DataType: "double", Precision: 255, Scale: 10}, false, false, "DOUBLE"},
		{source.ColumnInfo{DataType: "double", Precision: 255, Scale: 40}, false, false, "DOUBLE"},
		{source.ColumnInfo{DataType: "float", Precision: 5, Scale: 8}, false, false, "REAL"},
		{source.ColumnInfo{DataType: "varchar", Length: 100}, false, false, "VARCHAR2(100)"},
		{source.ColumnInfo{DataType: "varchar", Length: 100}, true, false, "VARCHAR2(100 CHAR)"},
		{source.ColumnInfo{DataType: "varchar", Length: 100}, false, true, "NVARCHAR2(100)"},
		{source.ColumnInfo{DataType: "char", Length: 8}, false, false, "CHAR(8)"},
		{source.ColumnInfo{DataType: "text"}, false, false, "TEXT"},
		{source.ColumnInfo{DataType: "mediumtext"}, false, false, "TEXT"},
		{source.ColumnInfo{DataType: "longtext"}, false, false, "TEXT"},
		{source.ColumnInfo{DataType: "json"}, false, false, "CLOB"},
		{source.ColumnInfo{DataType: "datetime"}, false, false, "TIMESTAMP"},
		{source.ColumnInfo{DataType: "date"}, false, false, "DATE"},
		{source.ColumnInfo{DataType: "blob"}, false, false, "BLOB"},
		{source.ColumnInfo{DataType: "bit"}, false, false, "NUMBER(1)"},
		{source.ColumnInfo{DataType: "enum"}, false, false, "VARCHAR2(255)"},
	}
	for _, c := range cases {
		got := MySQLToDameng(c.col, c.charInLength, c.useNvarchar2)
		assert.Equal(t, c.expected, got, "type=%s charInLen=%v nvarchar2=%v", c.col.DataType, c.charInLength, c.useNvarchar2)
	}
}

func TestMySQLToDamengCharacterLengths(t *testing.T) {
	cases := []struct {
		dataType string
		length   int64
		expected [4]string
	}{
		{"char", 0, [4]string{"CHAR(1)", "CHAR(1 CHAR)", "NVARCHAR2(1)", "NVARCHAR2(1)"}},
		{"char", -1, [4]string{"CHAR(1)", "CHAR(1 CHAR)", "NVARCHAR2(1)", "NVARCHAR2(1)"}},
		{"char", 8, [4]string{"CHAR(8)", "CHAR(8 CHAR)", "NVARCHAR2(8)", "NVARCHAR2(8)"}},
		{"varchar", 0, [4]string{"VARCHAR2(1)", "VARCHAR2(1 CHAR)", "NVARCHAR2(1)", "NVARCHAR2(1)"}},
		{"varchar", -1, [4]string{"VARCHAR2(1)", "VARCHAR2(1 CHAR)", "NVARCHAR2(1)", "NVARCHAR2(1)"}},
		{"varchar", 100, [4]string{"VARCHAR2(100)", "VARCHAR2(100 CHAR)", "NVARCHAR2(100)", "NVARCHAR2(100)"}},
	}
	options := []struct {
		name         string
		charInLength bool
		useNvarchar2 bool
	}{
		{"default", false, false},
		{"char_length", true, false},
		{"nvarchar2", false, true},
		{"nvarchar2_with_char_length", true, true},
	}
	for _, tc := range cases {
		for i, opt := range options {
			t.Run(fmt.Sprintf("%s/length=%d/%s", tc.dataType, tc.length, opt.name), func(t *testing.T) {
				col := source.ColumnInfo{DataType: tc.dataType, Length: tc.length}
				got := MySQLToDameng(col, opt.charInLength, opt.useNvarchar2)
				assert.Equal(t, tc.expected[i], got)
			})
		}
	}
}

// TestRegistryMySQLDameng 验证注册表已登记 mysql→dameng。
func TestRegistryMySQLDameng(t *testing.T) {
	m, ok := Get("mysql", "dameng")
	assert.True(t, ok)
	assert.NotNil(t, m)
}
