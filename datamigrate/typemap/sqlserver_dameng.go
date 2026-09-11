package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() { Register("sqlserver", "dameng", SQLServerToDameng) }

// SQLServerToDameng is paired with ValidateSQLServerDamengColumn. Unknown types
// deliberately return no SQL; callers must validate before executing any DDL.
func SQLServerToDameng(c source.ColumnInfo, charLength, national bool) string {
	switch strings.ToLower(c.DataType) {
	case "tinyint", "smallint":
		if c.IdentityInfo != nil {
			return "INT"
		} // DM IDENTITY supports INT/BIGINT
		return "SMALLINT"
	case "int":
		return "INT"
	case "bigint":
		return "BIGINT"
	case "bit":
		return "NUMBER(1)"
	case "decimal", "numeric":
		return fmt.Sprintf("NUMBER(%d,%d)", c.Precision, c.Scale)
	case "money":
		return "NUMBER(19,4)"
	case "smallmoney":
		return "NUMBER(10,4)"
	case "real":
		return "REAL"
	case "float":
		if c.Precision <= 24 {
			return "REAL"
		}
		return "DOUBLE"
	case "char", "nchar", "varchar", "nvarchar":
		if c.Length == -1 {
			return "CLOB"
		}
		if national {
			return fmt.Sprintf("NVARCHAR2(%d)", c.Length)
		}
		kind := "VARCHAR2"
		if strings.EqualFold(c.DataType, "char") || strings.EqualFold(c.DataType, "nchar") {
			kind = "CHAR"
		}
		if charLength {
			return fmt.Sprintf("%s(%d CHAR)", kind, c.Length)
		}
		return fmt.Sprintf("%s(%d)", kind, c.Length)
	case "text", "ntext", "xml":
		return "CLOB"
	case "binary", "varbinary", "image":
		return "BLOB"
	case "timestamp", "rowversion":
		return "VARBINARY(8)"
	case "uniqueidentifier":
		return "VARCHAR2(36)"
	case "date":
		return "DATE"
	case "smalldatetime":
		return "TIMESTAMP(0)"
	case "datetime":
		return "TIMESTAMP(3)"
	case "datetime2":
		return fmt.Sprintf("TIMESTAMP(%d)", c.DateTimePrecision)
	case "datetimeoffset":
		return fmt.Sprintf("TIMESTAMP(%d) WITH TIME ZONE", c.DateTimePrecision)
	case "time":
		return fmt.Sprintf("TIME(%d)", c.DateTimePrecision)
	}
	return ""
}

func ValidateSQLServerDamengColumn(c source.ColumnInfo) error {
	fail := func(s string) error { return fmt.Errorf("列 %s (%s): %s", c.Name, c.DataType, s) }
	if c.Generated != "" {
		return fail("暂不支持计算列")
	}
	if c.TypeKind != "" {
		return fail("暂不支持用户定义类型")
	}
	if SQLServerToDameng(c, false, false) == "" {
		return fail("不支持转换到达梦，禁止自动降级")
	}
	switch strings.ToLower(c.DataType) {
	case "decimal", "numeric":
		if c.Precision < 1 || c.Precision > 38 || c.Scale < 0 || c.Scale > c.Precision {
			return fail("数值精度或标度超出范围")
		}
	case "char", "nchar", "varchar", "nvarchar":
		if c.Length != -1 && (c.Length < 1 || c.Length > 8000) {
			return fail("字符长度超出支持范围")
		}
	case "datetime2", "datetimeoffset", "time":
		if c.DateTimePrecision < 0 || c.DateTimePrecision > 6 {
			return fail("DM8 兼容基线最多支持 6 位小数秒，禁止截断")
		}
	}
	if c.Extra == "auto_increment" && c.IdentityInfo == nil {
		return fail("缺少自增种子/步长元数据")
	}
	if c.IdentityInfo != nil {
		switch strings.ToLower(c.DataType) {
		case "tinyint", "smallint", "int", "bigint":
		default:
			return fail("自增列仅支持整数类型")
		}
		if c.IdentityInfo.Increment == 0 {
			return fail("自增步长不能为零")
		}
	}
	return nil
}
