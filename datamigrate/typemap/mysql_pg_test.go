package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/assert"
)

func TestMySQLToPG(t *testing.T) {
	cases := []struct {
		col      source.ColumnInfo
		expected string
	}{
		{source.ColumnInfo{DataType: "int"}, "int"},
		{source.ColumnInfo{DataType: "tinyint"}, "int"},
		{source.ColumnInfo{DataType: "mediumint"}, "int"},
		{source.ColumnInfo{DataType: "smallint"}, "int"},
		{source.ColumnInfo{DataType: "bigint"}, "bigint"},
		{source.ColumnInfo{DataType: "float"}, "decimal"},
		{source.ColumnInfo{DataType: "double"}, "decimal"},
		{source.ColumnInfo{DataType: "float", Precision: 8, Scale: 2}, "decimal(8,2)"},
		{source.ColumnInfo{DataType: "decimal", Precision: 10, Scale: 2}, "decimal(10,2)"},
		{source.ColumnInfo{DataType: "varchar", Length: 255}, "varchar(255)"},
		{source.ColumnInfo{DataType: "char", Length: 10}, "char(10)"},
		{source.ColumnInfo{DataType: "text"}, "text"},
		{source.ColumnInfo{DataType: "tinytext"}, "text"},
		{source.ColumnInfo{DataType: "mediumtext"}, "text"},
		{source.ColumnInfo{DataType: "longtext"}, "text"},
		{source.ColumnInfo{DataType: "datetime"}, "timestamp"},
		{source.ColumnInfo{DataType: "timestamp"}, "timestamp"},
		{source.ColumnInfo{DataType: "date"}, "date"},
		{source.ColumnInfo{DataType: "time"}, "time"},
		{source.ColumnInfo{DataType: "blob"}, "bytea"},
		{source.ColumnInfo{DataType: "tinyblob"}, "bytea"},
		{source.ColumnInfo{DataType: "mediumblob"}, "bytea"},
		{source.ColumnInfo{DataType: "longblob"}, "bytea"},
		{source.ColumnInfo{DataType: "binary"}, "bytea"},
		{source.ColumnInfo{DataType: "varbinary"}, "bytea"},
		{source.ColumnInfo{DataType: "json"}, "jsonb"},
		{source.ColumnInfo{DataType: "enum"}, "varchar(255)"},
		{source.ColumnInfo{DataType: "set"}, "text"},
		{source.ColumnInfo{DataType: "year"}, "int"},
		{source.ColumnInfo{DataType: "bit"}, "bit"},
	}
	for _, c := range cases {
		t.Run(c.col.DataType, func(t *testing.T) {
			assert.Equal(t, c.expected, MySQLToPG(c.col, false, false))
		})
	}
}
