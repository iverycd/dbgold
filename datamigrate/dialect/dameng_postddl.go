package dialect

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// SequenceStatements 达梦自增列以 IDENTITY 在建表时声明,这里生成「重置种子」语句,
// 把 IDENTITY 当前值推到源库当前 AUTO_INCREMENT 值,避免后续插入撞已导入的 id。
// 确切语法依达梦版本而定;失败由 Writer 降级处理(记日志,不使 job 失败)。
func (d *DaMengDialect) SequenceStatements(schema string, seq source.SequenceInfo) []Statement {
	sql := fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN "%s" RESTART WITH %d`,
		d.QualifyTable(schema, seq.TableName), seq.ColumnName, seq.StartValue)
	return []Statement{{SQL: sql}}
}

// IndexStatements 达梦索引/主键 DDL,不带 IF NOT EXISTS。
// 注意:达梦索引名在 schema 内全局唯一(不同于 MySQL 的表级作用域),
// 故非主键索引名加「表名_」前缀去重,避免多表同名索引(如 row_id、ix_tmp_autoinc)冲突。
func (d *DaMengDialect) IndexStatements(schema string, idx source.IndexInfo) []Statement {
	quotedCols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	cols := strings.Join(quotedCols, ", ")
	qn := d.QualifyTable(schema, idx.TableName)
	var sql string
	switch {
	case idx.IsPrimary:
		sql = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s)", qn, cols)
	case idx.IsUnique:
		sql = fmt.Sprintf(`CREATE UNIQUE INDEX "%s" ON %s (%s)`, d.uniqueIndexName(idx), qn, cols)
	default:
		sql = fmt.Sprintf(`CREATE INDEX "%s" ON %s (%s)`, d.uniqueIndexName(idx), qn, cols)
	}
	return []Statement{{SQL: sql}}
}

// uniqueIndexName 为达梦生成 schema 级唯一的索引名:「表名_索引名」。
func (d *DaMengDialect) uniqueIndexName(idx source.IndexInfo) string {
	return idx.TableName + "_" + idx.IndexName
}

// ForeignKeyStatements 达梦外键 DDL。达梦/Oracle 不支持 ON UPDATE 子句,予以丢弃;
// ON DELETE 仅在非 NO ACTION 时输出(NO ACTION 为默认,可省略)。
func (d *DaMengDialect) ForeignKeyStatements(schema string, fk source.FKInfo) []Statement {
	quotedCols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	quotedRefCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		quotedRefCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	sql := fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES %s (%s)`,
		d.QualifyTable(schema, fk.TableName), fk.ConstraintName,
		strings.Join(quotedCols, ", "),
		d.QualifyTable(schema, fk.RefTable),
		strings.Join(quotedRefCols, ", "))
	if od := strings.ToUpper(strings.TrimSpace(fk.OnDelete)); od != "" && od != "NO ACTION" {
		sql += " ON DELETE " + od
	}
	return []Statement{{SQL: sql}}
}

// ViewStatements 达梦视图 DDL。注意:MySQL 视图定义为 MySQL 方言,
// 在达梦下可能因函数/语法差异执行失败,失败由上层记入 report(已知限制)。
func (d *DaMengDialect) ViewStatements(schema string, view source.ViewInfo) []Statement {
	sql := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s", d.QualifyTable(schema, view.ViewName), view.Definition)
	return []Statement{{SQL: sql}}
}

// AdjustViewDefinition 把中间形式 gen_random_uuid() 替换为达梦的 sys_guid()。
func (d *DaMengDialect) AdjustViewDefinition(def string) string {
	return reGenRandomUUID.ReplaceAllString(def, "sys_guid()")
}
