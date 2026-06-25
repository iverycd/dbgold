package dialect

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/coldefault"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// MySQLDialect 实现 MySQL 目标库的 SQL 生成。
//
// 与 PostgreSQL 系的主要差异:
//   - 标识符用反引号 `name` 而非双引号。
//   - schema 概念即 database,QualifyTable 生成 `db`.`table`。
//   - 无独立 SEQUENCE 对象:自增在建表阶段以 AUTO_INCREMENT 列声明
//     (Caps.UsesSequences=false);Phase3 的 SequenceStatements 退化为
//     ALTER TABLE ... AUTO_INCREMENT=n 修正种子(容错)。
//   - 支持 DROP TABLE IF EXISTS,建表追加 ENGINE=InnoDB DEFAULT CHARSET=utf8mb4。
//   - 无 ALTER ... OWNER TO(对象 owner 即所在 database)。
//   - 目标按 MySQL 5.7 兼容生成:DEFAULT 表达式仅对日期时间列放行 CURRENT_TIMESTAMP。
type MySQLDialect struct{}

// NewMySQL 创建 MySQL 方言。
func NewMySQL() *MySQLDialect { return &MySQLDialect{} }

func (d *MySQLDialect) Name() string { return "mysql" }

func (d *MySQLDialect) Caps() Capabilities {
	return Capabilities{
		UsesSequences:       false, // 自增用 AUTO_INCREMENT 列声明,不建独立序列
		IdentityInsert:      false, // 自增列允许显式插入原值,无需开关
		SupportsDistribute:  false,
		SupportsChangeOwner: false,
	}
}

func (d *MySQLDialect) QuoteIdent(name string) string {
	// 反引号转义:列名/表名内的反引号需双写
	return fmt.Sprintf("`%s`", strings.ReplaceAll(name, "`", "``"))
}

func (d *MySQLDialect) QualifyTable(schema, table string) string {
	if schema == "" {
		return d.QuoteIdent(table)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

func (d *MySQLDialect) MapType(col source.ColumnInfo, srcType string, opt TypeOpt) string {
	if m, ok := typemap.Get(srcType, "mysql"); ok {
		return m(col, opt.CharInLength, opt.UseNvarchar2)
	}
	// 兜底:理论上不会发生(注册表已覆盖所有受支持组合)
	return strings.ToLower(col.DataType)
}

// CreateTableStatements 生成 MySQL 建表 DDL:
//   - DROP TABLE IF EXISTS(MySQL 原生支持)
//   - CREATE TABLE,自增列追加 AUTO_INCREMENT(种子由 Phase3 修正)
//   - 表选项 ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
func (d *MySQLDialect) CreateTableStatements(schema string, info *source.TableDDLInfo, srcType string, opt TypeOpt, name NameFunc) ([]Statement, error) {
	var cols []string
	for _, col := range info.Columns {
		myType := d.MapType(col, srcType, opt)
		colDef := fmt.Sprintf("%s %s", d.QuoteIdent(name(col.Name)), myType)
		if !col.IsNullable {
			colDef += " NOT NULL"
		}
		if col.Extra == "auto_increment" {
			colDef += " AUTO_INCREMENT"
		} else if col.Default != nil {
			if clause := d.columnDefaultClause(coldefault.Strip(srcType, *col.Default), myType); clause != "" {
				colDef += clause
			}
		}
		cols = append(cols, "  "+colDef)
	}
	qn := d.QualifyTable(schema, name(info.TableName))
	dropSQL := fmt.Sprintf("DROP TABLE IF EXISTS %s", qn)
	createSQL := fmt.Sprintf("CREATE TABLE %s (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		qn, strings.Join(cols, ",\n"))
	return []Statement{{SQL: dropSQL}, {SQL: createSQL}}, nil
}

// columnDefaultClause 生成 MySQL 列默认值子句(含前导空格),无默认值返回空串。
// myType 为已映射的目标列类型,用于门控 CURRENT_TIMESTAMP(仅 datetime/timestamp 列合法)。
func (d *MySQLDialect) columnDefaultClause(def, myType string) string {
	if def == "" {
		return ""
	}
	upper := strings.ToUpper(strings.TrimSpace(def))
	lowerType := strings.ToLower(myType)
	isDateTime := strings.HasPrefix(lowerType, "datetime") || strings.HasPrefix(lowerType, "timestamp")

	switch upper {
	case "CURRENT_TIMESTAMP", "NOW()", "SYSDATE", "GETDATE()":
		if isDateTime {
			return " DEFAULT CURRENT_TIMESTAMP"
		}
		// 非日期时间列不能用函数默认值,降级为 NULL(MySQL 5.7 限制)
		return " DEFAULT NULL"
	case "CURRENT_DATE":
		if isDateTime {
			return " DEFAULT CURRENT_TIMESTAMP"
		}
		return " DEFAULT NULL"
	case "NULL":
		return " DEFAULT NULL"
	case "TRUE":
		return " DEFAULT 1"
	case "FALSE":
		return " DEFAULT 0"
	case "SYS_GUID()", "NEWID()", "UUID()", "USER", "USER()":
		// MySQL 5.7 不支持函数/UUID 默认值,降级为 NULL
		return " DEFAULT NULL"
	}

	// MySQL 位串字面量 b'0' / B'1' → 裸数字
	if len(def) >= 3 && (def[0] == 'b' || def[0] == 'B') && def[1] == '\'' && def[len(def)-1] == '\'' {
		return fmt.Sprintf(" DEFAULT %d", bitsToIntMySQL(def[2:len(def)-1]))
	}

	// TEXT/BLOB/JSON 列在 MySQL 5.7 不能有默认值,直接跳过
	if isNoDefaultType(lowerType) {
		return ""
	}

	// 纯数值字面量:不加引号
	if isNumericLiteral(def) {
		return fmt.Sprintf(" DEFAULT %s", def)
	}
	// 其余函数调用(以右括号结尾):MySQL 5.7 仅 CURRENT_TIMESTAMP 可作默认值,
	// 其他函数无法作为字面量默认值,降级 NULL
	if strings.HasSuffix(upper, ")") {
		return " DEFAULT NULL"
	}
	// 普通字符串字面量:加单引号并转义
	return fmt.Sprintf(" DEFAULT '%s'", strings.ReplaceAll(def, "'", "''"))
}

// isNoDefaultType 判断目标列类型是否为 MySQL 5.7 中不允许默认值的大对象类型。
func isNoDefaultType(lowerType string) bool {
	for _, t := range []string{"text", "blob", "json", "geometry"} {
		if strings.Contains(lowerType, t) {
			return true
		}
	}
	return false
}

// isNumericLiteral 判断字符串是否为纯数值字面量(整数或小数,可带正负号)。
func isNumericLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	dot := false
	for i, c := range s {
		switch {
		case c == '-' || c == '+':
			if i != 0 {
				return false
			}
		case c == '.':
			if dot {
				return false
			}
			dot = true
		case c < '0' || c > '9':
			return false
		}
	}
	return true
}

// bitsToIntMySQL 把位串(如 "0"/"1"/"101")转为十进制整数。
func bitsToIntMySQL(bits string) int64 {
	var n int64
	for _, c := range bits {
		n <<= 1
		if c == '1' {
			n |= 1
		}
	}
	return n
}
