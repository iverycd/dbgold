package dialect

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/source"
)

// 以下 PostDDL 方法生成 MySQL 目标库的 Phase3 语句,
// 作为「报告展示」与「实际执行」的唯一真相来源(与 MySQLWriter 同源)。

// SequenceStatements 修正自增列种子。
// MySQL 无独立 SEQUENCE 对象,自增已在建表时以 AUTO_INCREMENT 列声明,
// 这里只生成 ALTER TABLE ... AUTO_INCREMENT=n 修正下一个自增起始值。
// 若该列实际不是自增列,执行会报错,由 Writer 容错忽略。
func (d *MySQLDialect) SequenceStatements(schema string, seq source.SequenceInfo) []Statement {
	start := seq.StartValue
	if start < 1 {
		start = 1
	}
	sql := fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d",
		d.QualifyTable(schema, seq.TableName), start)
	return []Statement{{SQL: sql}}
}

// IndexStatements 生成主键 / 唯一索引 / 普通索引。
func (d *MySQLDialect) IndexStatements(schema string, idx source.IndexInfo) []Statement {
	quotedCols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		quotedCols[i] = d.QuoteIdent(c)
	}
	cols := strings.Join(quotedCols, ", ")
	qn := d.QualifyTable(schema, idx.TableName)

	var sql string
	switch {
	case idx.IsPrimary:
		sql = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s)", qn, cols)
	case idx.IsUnique:
		sql = fmt.Sprintf("ALTER TABLE %s ADD UNIQUE INDEX %s (%s)",
			qn, d.QuoteIdent(idx.IndexName), cols)
	default:
		sql = fmt.Sprintf("ALTER TABLE %s ADD INDEX %s (%s)",
			qn, d.QuoteIdent(idx.IndexName), cols)
	}
	return []Statement{{SQL: sql}}
}

// ForeignKeyStatements 生成外键约束。MySQL 支持 ON DELETE / ON UPDATE。
func (d *MySQLDialect) ForeignKeyStatements(schema string, fk source.FKInfo) []Statement {
	quotedCols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quotedCols[i] = d.QuoteIdent(c)
	}
	quotedRefCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		quotedRefCols[i] = d.QuoteIdent(c)
	}
	sql := fmt.Sprintf(
		"ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		d.QualifyTable(schema, fk.TableName), d.QuoteIdent(fk.ConstraintName),
		strings.Join(quotedCols, ", "),
		d.QualifyTable(schema, fk.RefTable),
		strings.Join(quotedRefCols, ", "))
	if on := normalizeFKAction(fk.OnDelete); on != "" {
		sql += " ON DELETE " + on
	}
	if on := normalizeFKAction(fk.OnUpdate); on != "" {
		sql += " ON UPDATE " + on
	}
	return []Statement{{SQL: sql}}
}

// normalizeFKAction 规整外键动作。MySQL 不支持 RESTRICT 之外的某些写法,
// 空或 NO ACTION/RESTRICT 时省略(MySQL 默认即 RESTRICT)。
func normalizeFKAction(action string) string {
	a := strings.ToUpper(strings.TrimSpace(action))
	switch a {
	case "", "NO ACTION", "RESTRICT":
		return ""
	default:
		return a
	}
}

// ViewStatements 生成 CREATE OR REPLACE VIEW。
func (d *MySQLDialect) ViewStatements(schema string, view source.ViewInfo) []Statement {
	sql := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s",
		d.QualifyTable(schema, view.ViewName), view.Definition)
	return []Statement{{SQL: sql}}
}

// CommentStatements MySQL 不支持 COMMENT ON 语句。
//   - 表注释:ALTER TABLE ... COMMENT = '...' 可行,不需列类型。
//   - 列注释:必须用 ALTER TABLE ... MODIFY COLUMN <col> <type> COMMENT '...' 重写整列定义,
//     需要列的完整类型,而 CommentInfo 不携带类型信息。本次作为已知限制不实现,返回空切片,
//     由调用方(migrator)跳过、不计入失败。
func (d *MySQLDialect) CommentStatements(schema string, cm source.CommentInfo) []Statement {
	if cm.ColumnName != "" {
		return []Statement{}
	}
	val := strings.ReplaceAll(cm.Comment, "'", "''")
	sql := fmt.Sprintf("ALTER TABLE %s COMMENT = '%s'", d.QualifyTable(schema, cm.TableName), val)
	return []Statement{{SQL: sql}}
}

var reGenRandomUUIDMySQL = regexp.MustCompile(`(?i)\bgen_random_uuid\s*\(\s*\)`)

// AdjustViewDefinition 把视图定义中的中间形式 UUID 函数替换为 MySQL 的 uuid()。
func (d *MySQLDialect) AdjustViewDefinition(def string) string {
	return reGenRandomUUIDMySQL.ReplaceAllString(def, "uuid()")
}
