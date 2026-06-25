package typemap

import (
	"dbgold/datamigrate/source"
	"fmt"
	"strings"
)

func init() {
	Register("oracle", "mysql", OracleToMySQL)
}

// MySQL VARCHAR/CHAR 长度上限(字符数)。
// MySQL 单行上限 65535 字节,utf8mb4 每字符最多 4 字节,
// 故单个 varchar 安全字符上限约 16000;超过则降级为 TEXT 系列。
// CHAR 上限固定 255 字符。
const (
	mysqlVarcharMaxChars = 16000 // 超过降级 TEXT
	mysqlCharMaxChars    = 255   // 超过降级 VARCHAR
)

// OracleToMySQL 将 Oracle 列类型映射为 MySQL 类型字符串(目标 MySQL 5.7+,utf8mb4)。
//
// 设计要点(对比文档做的改进):
//   - NUMBER(p,0) 按 precision 分级映射整型(与项目 OracleToPG 一致),
//     不依赖文档中的 AVG_COL_LEN(本项目 Reader 未采集该统计信息)。
//   - VARCHAR2/CHAR 超长自动降级 TEXT/VARCHAR,避免 MySQL 行长/列长限制导致建表失败。
//   - 补全文档缺失的类型:BINARY_FLOAT/DOUBLE、TIMESTAMP WITH TIME ZONE、
//     INTERVAL、XMLTYPE、ROWID、BOOLEAN、FLOAT(p) 等。
//
// charInLength / useNvarchar2 对 MySQL 目标无意义(MySQL VARCHAR 长度即字符数、
// 无 nvarchar2 类型),保留签名以符合 Mapper 接口。
func OracleToMySQL(col source.ColumnInfo, charInLength, useNvarchar2 bool) string {
	dt := strings.ToUpper(strings.TrimSpace(col.DataType))
	base := dt
	if idx := strings.Index(dt, "("); idx != -1 {
		base = strings.TrimSpace(dt[:idx])
	}

	// INTERVAL 类型可能带精度括号(如 INTERVAL YEAR(2) TO MONTH),
	// 截断到首个 "(" 会破坏匹配,故用前缀判断优先处理。
	if strings.HasPrefix(base, "INTERVAL") {
		return "varchar(50)"
	}
	// TIMESTAMP WITH (LOCAL) TIME ZONE 同理:可能带精度括号。
	if strings.HasPrefix(base, "TIMESTAMP") {
		return "datetime(6)"
	}

	switch base {
	case "NUMBER", "NUMERIC", "DECIMAL":
		// 整数(scale=0):按 precision 分级
		if col.Precision > 0 && col.Scale == 0 {
			switch {
			case col.Precision <= 4:
				return "smallint"
			case col.Precision <= 9:
				return "int"
			case col.Precision <= 18:
				return "bigint"
			default:
				return fmt.Sprintf("decimal(%d,0)", col.Precision)
			}
		}
		// 定点小数
		if col.Precision > 0 {
			scale := col.Scale
			if scale < 0 {
				scale = 0
			}
			// MySQL decimal 精度上限 65,标度上限 30
			p := col.Precision
			if p > 65 {
				p = 65
			}
			if scale > 30 {
				scale = 30
			}
			return fmt.Sprintf("decimal(%d,%d)", p, scale)
		}
		// 无精度的 NUMBER:Oracle 浮点数,用 double 兜底保精度
		return "double"

	case "FLOAT", "DOUBLE PRECISION", "BINARY_DOUBLE":
		return "double"

	case "BINARY_FLOAT", "REAL":
		return "float"

	case "INTEGER", "INT", "SMALLINT":
		return "int"

	case "CHAR", "NCHAR", "CHARACTER":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlCharMaxChars {
			return fmt.Sprintf("varchar(%d)", n)
		}
		return fmt.Sprintf("char(%d)", n)

	case "VARCHAR2", "NVARCHAR2", "VARCHAR":
		n := col.Length
		if n <= 0 {
			n = 1
		}
		if n > mysqlVarcharMaxChars {
			return "text"
		}
		return fmt.Sprintf("varchar(%d)", n)

	case "UROWID", "ROWID":
		n := col.Length
		if n <= 0 {
			n = 100
		}
		return fmt.Sprintf("varchar(%d)", n)

	case "CLOB", "NCLOB", "LONG":
		return "longtext"

	case "BLOB", "RAW", "LONG RAW":
		return "longblob"

	// Oracle DATE 含时分秒,映射到 MySQL datetime
	case "DATE":
		return "datetime"

	case "BOOLEAN":
		return "tinyint(1)"

	case "XMLTYPE":
		return "longtext"

	default:
		return strings.ToLower(dt)
	}
}
