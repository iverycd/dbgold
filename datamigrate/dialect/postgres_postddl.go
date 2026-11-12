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
func (d *PostgresDialect) SequenceStatements(schema string, seq source.SequenceInfo, owner string) []Statement {
	seqBase := fmt.Sprintf("seq_%s_%s", seq.TableName, seq.ColumnName)
	quotedSeq := d.qualified(schema, seqBase)
	createSQL := fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s INCREMENT BY 1 START %d", quotedSeq, seq.StartValue)
	alterSQL := fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN "%s" SET DEFAULT nextval('%s')`,
		d.qualified(schema, seq.TableName), seq.ColumnName, quotedSeq)
	ownedBySQL := fmt.Sprintf(`ALTER SEQUENCE %s OWNED BY %s."%s"`,
		quotedSeq, d.qualified(schema, seq.TableName), seq.ColumnName)
	stmts := []Statement{{SQL: createSQL}, {SQL: alterSQL}}
	if owner != "" {
		stmts = append(stmts, Statement{SQL: fmt.Sprintf("ALTER SEQUENCE %s OWNER TO %s", quotedSeq, d.QuoteIdent(owner))})
	}
	return append(stmts, Statement{SQL: ownedBySQL})
}

func (d *PostgresDialect) IndexName(idx source.IndexInfo) string {
	if idx.TargetIndexName != "" {
		return idx.TargetIndexName
	}
	return idx.IndexName
}

func (d *PostgresDialect) IndexStatements(schema string, idx source.IndexInfo) []Statement {
	if idx.PGFulltext {
		if idx.Unsupported != "" || idx.IsPrimary || idx.IsUnique || len(idx.Columns) == 0 {
			return nil
		}
		parts := make([]string, len(idx.Columns))
		for i, column := range idx.Columns {
			parts[i] = fmt.Sprintf("COALESCE(%s::pg_catalog.text, ''::pg_catalog.text)", d.QuoteIdent(column))
		}
		document := strings.Join(parts, " OPERATOR(pg_catalog.||) ' '::pg_catalog.text OPERATOR(pg_catalog.||) ")
		return []Statement{{SQL: fmt.Sprintf("CREATE INDEX %s ON %s USING gin (pg_catalog.to_tsvector('pg_catalog.simple'::pg_catalog.regconfig, %s) pg_catalog.tsvector_ops);",
			d.QuoteIdent(d.IndexName(idx)), d.qualified(schema, idx.TableName), document)}}
	}
	if d.name == "gaussdb" && idx.GaussDB != nil {
		schema = idx.GaussDB.Base.Schema
	}
	if d.name == "gaussdb" && idx.GaussDB != nil && idx.GaussDB.Global {
		cols := strings.Join(orderedIndexColumns(d, idx), ", ")
		table := d.qualified(schema, idx.TableName)
		if idx.IsPrimary {
			return []Statement{{SQL: fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s) BY GLOBAL INDEX;", table, cols)}}
		}
		if idx.PGUniqueConstraint {
			return []Statement{{SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s) BY GLOBAL INDEX;", table, d.QuoteIdent(d.IndexName(idx)), cols)}}
		}
		keys := make([]string, len(idx.GaussDB.Columns))
		for i, key := range idx.GaussDB.Columns {
			keys[i] = d.QuoteIdent(key)
		}
		return []Statement{{SQL: fmt.Sprintf("CREATE GLOBAL UNIQUE INDEX %s ON %s USING ubtree (%s) DISTRIBUTE BY HASH (%s);", d.QuoteIdent(d.IndexName(idx)), table, cols, strings.Join(keys, ", "))}}
	}
	if idx.PGUniqueConstraint {
		cols := make([]string, len(idx.Columns))
		for i, column := range idx.Columns {
			cols[i] = d.QuoteIdent(column)
		}
		return []Statement{{SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);",
			d.qualified(schema, idx.TableName), d.QuoteIdent(d.IndexName(idx)), strings.Join(cols, ", "))}}
	}
	if strings.EqualFold(idx.Method, "gist") {
		cols := make([]string, len(idx.Columns))
		for i, column := range idx.Columns {
			cols[i] = d.QuoteIdent(column)
			if idx.PGSpatialSchema != "" && idx.PGSpatialOpClass != "" {
				cols[i] += " " + d.QuoteIdent(idx.PGSpatialSchema) + "." + d.QuoteIdent(idx.PGSpatialOpClass)
			}
		}
		// Spatial indexes always create strictly; the writer verifies a same-name
		// index's actual table, keys, method and operator class before reusing it.
		return []Statement{{SQL: fmt.Sprintf("CREATE INDEX %s ON %s USING gist (%s);", d.QuoteIdent(d.IndexName(idx)), d.qualified(schema, idx.TableName), strings.Join(cols, ", "))}}
	}
	cols := strings.Join(orderedIndexColumns(d, idx), ", ")
	table := d.qualified(schema, idx.TableName)
	if idx.IsPrimary {
		return []Statement{{SQL: fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);", table, cols)}}
	}
	unique := ""
	if idx.IsUnique {
		unique = "UNIQUE "
	}
	ifAbsent := "IF NOT EXISTS "
	if len(idx.MySQLPrefixLengths) > 0 || idx.TargetIndexName != "" || idx.GaussDB != nil {
		// Let the writer verify duplicate definitions before reporting success.
		ifAbsent = ""
	}
	return []Statement{{SQL: fmt.Sprintf("CREATE %sINDEX %s%s ON %s (%s);",
		unique, ifAbsent, d.QuoteIdent(d.IndexName(idx)), table, cols)}}
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
