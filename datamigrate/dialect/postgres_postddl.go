package dialect

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/source"
)

// 以下 PostDDL 方法复刻 target/postgres.go 中 writer 实际执行的 SQL,
// 作为「报告展示」与「实际执行」的唯一真相来源。
// 注意:writer 原先用 IF NOT EXISTS、带 schema 前缀,与 report.go 旧的
// IndexDDL/SequenceDDL/FKDDL(不带 schema)不同。统一以 writer 行为为准。

func (d *PostgresDialect) qualified(schema, table string) string {
	return d.QualifyTable(schema, table)
}

// SequenceStatements 复刻 PostgresWriter.CreateSequence(postgres.go:111-130)。
func (d *PostgresDialect) SequenceStatements(schema string, seq source.SequenceInfo) []Statement {
	seqBase := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	var quotedSeq, nextvalArg string
	if schema != "" {
		quotedSeq = fmt.Sprintf(`"%s"."%s"`, schema, seqBase)
		nextvalArg = fmt.Sprintf(`%s."%s"`, schema, seqBase)
	} else {
		quotedSeq = fmt.Sprintf(`"%s"`, seqBase)
		nextvalArg = fmt.Sprintf(`"%s"`, seqBase)
	}
	createSQL := fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s INCREMENT BY 1 START %d", quotedSeq, seq.StartValue)
	alterSQL := fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN "%s" SET DEFAULT nextval('%s')`,
		d.qualified(schema, seq.TableName), seq.ColumnName, nextvalArg)
	return []Statement{{SQL: createSQL}, {SQL: alterSQL}}
}

// IndexStatements 复刻 PostgresWriter.CreateIndex(postgres.go:132-150)。
func (d *PostgresDialect) IndexStatements(schema string, idx source.IndexInfo) []Statement {
	quotedCols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	cols := strings.Join(quotedCols, ", ")
	var ddl string
	if idx.IsPrimary {
		ddl = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);", d.qualified(schema, idx.TableName), cols)
	} else if idx.IsUnique {
		ddl = fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS "%s" ON %s (%s);`,
			idx.IndexName, d.qualified(schema, idx.TableName), cols)
	} else {
		ddl = fmt.Sprintf(`CREATE INDEX IF NOT EXISTS "%s" ON %s (%s);`,
			idx.IndexName, d.qualified(schema, idx.TableName), cols)
	}
	return []Statement{{SQL: ddl}}
}

// ForeignKeyStatements 复刻 PostgresWriter.CreateForeignKey(postgres.go:152-170)。
func (d *PostgresDialect) ForeignKeyStatements(schema string, fk source.FKInfo) []Statement {
	quotedCols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	quotedRefCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		quotedRefCols[i] = fmt.Sprintf(`"%s"`, c)
	}
	ddl := fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s;`,
		d.qualified(schema, fk.TableName), fk.ConstraintName,
		strings.Join(quotedCols, ", "),
		d.qualified(schema, fk.RefTable),
		strings.Join(quotedRefCols, ", "),
		fk.OnDelete, fk.OnUpdate)
	return []Statement{{SQL: ddl}}
}

// ViewStatements 复刻 PostgresWriter.CreateView 拼出的 CREATE OR REPLACE VIEW 文本
// (postgres.go:172-173)。SET LOCAL search_path 的事务包装属 I/O,留在 Writer。
func (d *PostgresDialect) ViewStatements(schema string, view source.ViewInfo) []Statement {
	ddl := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s;", d.qualified(schema, view.ViewName), view.Definition)
	return []Statement{{SQL: ddl}}
}

// CommentStatements 生成 PostgreSQL 系的 COMMENT ON 语句。
// 表注释作用于表;列注释作用于列,均不需要列类型。
func (d *PostgresDialect) CommentStatements(schema string, cm source.CommentInfo) []Statement {
	val := strings.ReplaceAll(cm.Comment, "'", "''")
	var ddl string
	if cm.ColumnName == "" {
		ddl = fmt.Sprintf("COMMENT ON TABLE %s IS '%s';", d.qualified(schema, cm.TableName), val)
	} else {
		ddl = fmt.Sprintf(`COMMENT ON COLUMN %s."%s" IS '%s';`,
			d.qualified(schema, cm.TableName), cm.ColumnName, val)
	}
	return []Statement{{SQL: ddl}}
}

var reGenRandomUUID = regexp.MustCompile(`(?i)\bgen_random_uuid\s*\(\s*\)`)

// AdjustViewDefinition 复刻 migrator.adjustViewUUID(行59-68)。
func (d *PostgresDialect) AdjustViewDefinition(def string) string {
	switch d.name {
	case "gaussdb":
		return reGenRandomUUID.ReplaceAllString(def, "uuid()")
	case "seabox":
		return reGenRandomUUID.ReplaceAllString(def, "sys_guid()")
	default:
		return def
	}
}
