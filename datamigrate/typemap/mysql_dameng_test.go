package typemap

import (
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
		{source.ColumnInfo{DataType: "decimal"}, false, false, "NUMBER"},
		{source.ColumnInfo{DataType: "double"}, false, false, "DECIMAL"},
		{source.ColumnInfo{DataType: "float", Precision: 8, Scale: 2}, false, false, "DECIMAL(8,2)"},
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

// TestRegistryMySQLDameng 验证注册表已登记 mysql→dameng。
func TestRegistryMySQLDameng(t *testing.T) {
	m, ok := Get("mysql", "dameng")
	assert.True(t, ok)
	assert.NotNil(t, m)
}
