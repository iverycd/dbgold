package dialect

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/coldefault"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// PostgresDialect 实现 PostgreSQL 系目标库(postgres/gaussdb/seabox)的 SQL 生成。
// 行为与重构前散落在 migrator.go / report.go / target 的 PG 拼串逻辑逐字符等价。
//
// gaussdb / seabox 与 PG 仅在「函数默认值映射」和「视图 UUID 函数」上有差异,
// 通过 name 字段区分,共用同一套拼串逻辑。
type PostgresDialect struct {
	name string // "postgres" | "gaussdb" | "seabox"
}

// NewPostgres 创建 PostgreSQL 系方言。dbType 取 "postgres"/"gaussdb"/"seabox"。
func NewPostgres(dbType string) *PostgresDialect {
	if dbType == "" {
		dbType = "postgres"
	}
	return &PostgresDialect{name: dbType}
}

func (d *PostgresDialect) Name() string { return d.name }

func (d *PostgresDialect) Caps() Capabilities {
	return Capabilities{
		UsesSequences:       true,
		IdentityInsert:      false,
		SupportsDistribute:  d.name == "gaussdb",
		SupportsChangeOwner: true,
	}
}

func (d *PostgresDialect) QuoteIdent(name string) string {
	return fmt.Sprintf(`"%s"`, name)
}

func (d *PostgresDialect) QualifyTable(schema, table string) string {
	if schema == "" {
		return fmt.Sprintf(`"%s"`, table)
	}
	return fmt.Sprintf(`"%s"."%s"`, schema, table)
}

func (d *PostgresDialect) MapType(col source.ColumnInfo, srcType string, opt TypeOpt) string {
	if m, ok := typemap.Get(srcType, d.name); ok {
		return m(col, opt.CharInLength, opt.UseNvarchar2)
	}
	// 兜底:理论上不会发生(注册表已覆盖所有受支持组合)
	return col.DataType
}

var reBitLiteral = regexp.MustCompile(`(?i)^b'[01]+'$`)

// CreateTableStatements 复刻 migrator.buildCreateTableDDL(行242-289)的 PG 拼串逻辑。
func (d *PostgresDialect) CreateTableStatements(schema string, info *source.TableDDLInfo, srcType string, opt TypeOpt, name NameFunc) ([]Statement, error) {
	var cols []string
	for _, col := range info.Columns {
		pgType := d.MapType(col, srcType, opt)
		colDef := fmt.Sprintf(`"%s" %s`, name(col.Name), pgType)
		if !col.IsNullable {
			colDef += " NOT NULL"
		}
		if col.Default != nil && col.Extra != "auto_increment" {
			def := coldefault.Strip(srcType, *col.Default)
			// MySQL 位串字面量 b'0' / b'1' → PostgreSQL B'0' / B'1'
			if reBitLiteral.MatchString(def) {
				colDef += fmt.Sprintf(" DEFAULT B'%s'", def[2:len(def)-1])
			} else if isFunctionDefault(def) {
				colDef += fmt.Sprintf(" DEFAULT %s", d.functionDefault(def))
			} else {
				colDef += fmt.Sprintf(" DEFAULT '%s'", strings.ReplaceAll(def, "'", "''"))
			}
		}
		cols = append(cols, "  "+colDef)
	}
	tblName := name(info.TableName)
	var qualifiedName string
	if schema != "" {
		qualifiedName = fmt.Sprintf(`"%s"."%s"`, schema, tblName)
	} else {
		qualifiedName = fmt.Sprintf(`"%s"`, tblName)
	}
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE;\nCREATE TABLE %s (\n%s\n);",
		qualifiedName, qualifiedName, strings.Join(cols, ",\n"))
	return []Statement{{SQL: ddl}}, nil
}

// isFunctionDefault 判断默认值是否为函数或关键字(不应加引号)。
// 复刻 migrator.isFunctionDefault(行696)。
func isFunctionDefault(def string) bool {
	upper := strings.ToUpper(strings.TrimSpace(def))
	keywords := []string{
		"CURRENT_TIMESTAMP", "NOW()", "CURRENT_DATE", "CURRENT_TIME",
		"NULL", "TRUE", "FALSE",
	}
	for _, kw := range keywords {
		if upper == kw {
			return true
		}
	}
	return strings.HasSuffix(upper, ")")
}

// functionDefault 将函数默认值映射到目标库等价形式。
// 复刻 migrator.pgFunctionDefault(行712),按方言名分支处理 UUID 函数。
func (d *PostgresDialect) functionDefault(def string) string {
	upper := strings.ToUpper(strings.TrimSpace(def))
	switch upper {
	case "CURRENT_TIMESTAMP", "NOW()", "GETDATE()":
		return "CURRENT_TIMESTAMP"
	case "CURRENT_DATE":
		return "CURRENT_DATE"
	case "CURRENT_TIME":
		return "CURRENT_TIME"
	case "NULL":
		return "NULL"
	case "TRUE":
		return "TRUE"
	case "FALSE":
		return "FALSE"
	case "NEWID()", "UUID()":
		switch d.name {
		case "gaussdb":
			return "uuid()"
		case "seabox":
			return "sys_guid()"
		default:
			return "gen_random_uuid()"
		}
	default:
		return def
	}
}
