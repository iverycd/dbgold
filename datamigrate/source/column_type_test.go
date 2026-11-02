package source

import "testing"

func TestFormatColumnTypePreservesOriginalDeclaration(t *testing.T) {
	for _, test := range []struct {
		name string
		col  ColumnInfo
		want string
	}{
		{"varchar length", ColumnInfo{DataType: "varchar", Length: 6000}, "varchar(6000)"},
		{"oracle length", ColumnInfo{DataType: "VARCHAR2", Length: 6000}, "VARCHAR2(6000)"},
		{"oracle char semantics", ColumnInfo{DataType: "VARCHAR2", Length: 20, CharUsed: "C"}, "VARCHAR2(20 CHAR)"},
		{"oracle bare formatted type", ColumnInfo{DataType: "VARCHAR2", FormattedType: "VARCHAR2", Length: 100}, "VARCHAR2(100)"},
		{"decimal", ColumnInfo{DataType: "decimal", Precision: 18, Scale: 2}, "decimal(18,2)"},
		{"number negative scale", ColumnInfo{DataType: "NUMBER", Precision: 10, Scale: -2}, "NUMBER(10,-2)"},
		{"number unspecified", ColumnInfo{DataType: "NUMBER"}, "NUMBER"},
		{"sqlserver max", ColumnInfo{DataType: "nvarchar", Length: -1}, "nvarchar(max)"},
		{"mysql declaration", ColumnInfo{DataType: "decimal", FormattedType: "decimal(18,2) unsigned", Precision: 18, Scale: 2}, "decimal(18,2) unsigned"},
		{"mysql integer modifiers", ColumnInfo{DataType: "bigint", FormattedType: "bigint unsigned", Precision: 20}, "bigint unsigned"},
		{"postgres declaration", ColumnInfo{DataType: "varchar", FormattedType: "character varying(42)", Length: 42}, "character varying(42)"},
		{"timestamp", ColumnInfo{DataType: "TIMESTAMP(6) WITH TIME ZONE", DateTimePrecision: 6}, "TIMESTAMP(6) WITH TIME ZONE"},
		{"separate timestamp precision", ColumnInfo{DataType: "timestamp", DateTimePrecision: 3}, "timestamp(3)"},
		{"clob", ColumnInfo{DataType: "CLOB", Length: 4294967295}, "CLOB"},
		{"int", ColumnInfo{DataType: "int", Precision: 10}, "int"},
		{"user type", ColumnInfo{DataType: "MyType", TypeKind: "user_defined", TypeSchema: "MixedOwner"}, "MixedOwner.MyType"},
		{"empty", ColumnInfo{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := FormatColumnType(test.col); got != test.want {
				t.Fatalf("FormatColumnType(%+v) = %q, want %q", test.col, got, test.want)
			}
		})
	}
}
