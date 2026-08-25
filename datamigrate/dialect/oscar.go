package dialect

import (
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/coldefault"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// OscarDialect is deliberately separate from PostgresDialect. Similar-looking
// syntax is kept here so Oscar compatibility changes cannot affect PG targets.
type OscarDialect struct{}

func NewOscar() *OscarDialect        { return &OscarDialect{} }
func (d *OscarDialect) Name() string { return "oscar" }
func (d *OscarDialect) Caps() Capabilities {
	return Capabilities{UsesSequences: true, SupportsChangeOwner: true}
}

func (d *OscarDialect) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (d *OscarDialect) QualifyTable(schema, table string) string {
	if schema == "" {
		return d.QuoteIdent(table)
	}
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

func (d *OscarDialect) MapType(col source.ColumnInfo, srcType string, opt TypeOpt) string {
	if mapper, ok := typemap.Get(srcType, "oscar"); ok {
		return mapper(col, opt.CharInLength, opt.UseNvarchar2)
	}
	return strings.ToUpper(col.DataType)
}

var oscarNumericLiteral = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$`)

func (d *OscarDialect) CreateTableStatements(schema string, info *source.TableDDLInfo, srcType string, opt TypeOpt, name NameFunc) ([]Statement, error) {
	columns := make([]string, 0, len(info.Columns))
	for _, col := range info.Columns {
		definition := d.QuoteIdent(name(col.Name)) + " " + d.MapType(col, srcType, opt)
		if !col.IsNullable {
			definition += " NOT NULL"
		}
		if col.Default != nil && col.Extra != "auto_increment" {
			value := strings.TrimSpace(coldefault.Strip(srcType, *col.Default))
			switch {
			case reBitLiteral.MatchString(value):
				if value[len(value)-2] == '1' {
					definition += " DEFAULT TRUE"
				} else {
					definition += " DEFAULT FALSE"
				}
			case oscarNumericLiteral.MatchString(value), isFunctionDefault(value):
				definition += " DEFAULT " + d.functionDefault(value)
			default:
				definition += " DEFAULT '" + strings.ReplaceAll(value, "'", "''") + "'"
			}
		}
		columns = append(columns, "  "+definition)
	}
	qualified := d.QualifyTable(schema, name(info.TableName))
	return []Statement{
		{SQL: "DROP TABLE IF EXISTS " + qualified + " CASCADE"},
		{SQL: fmt.Sprintf("CREATE TABLE %s (\n%s\n)", qualified, strings.Join(columns, ",\n"))},
	}, nil
}

func (d *OscarDialect) functionDefault(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "NOW()", "GETDATE()":
		return "CURRENT_TIMESTAMP"
	case "TRUE":
		return "TRUE"
	case "FALSE":
		return "FALSE"
	case "NULL":
		return "NULL"
	default:
		return value
	}
}

func (d *OscarDialect) SequenceStatements(schema string, seq source.SequenceInfo) []Statement {
	seqName := "seq_" + seq.TableName + "_" + seq.ColumnName
	qualifiedSeq := d.QualifyTable(schema, seqName)
	return []Statement{
		{SQL: "DROP SEQUENCE IF EXISTS " + qualifiedSeq},
		{SQL: fmt.Sprintf("CREATE SEQUENCE %s INCREMENT BY 1 START WITH %d", qualifiedSeq, seq.StartValue)},
		{SQL: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT nextval('%s')", d.QualifyTable(schema, seq.TableName), d.QuoteIdent(seq.ColumnName), qualifiedSeq)},
	}
}

func quotedOscarColumns(d *OscarDialect, columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = d.QuoteIdent(column)
	}
	return strings.Join(quoted, ", ")
}

func (d *OscarDialect) IndexStatements(schema string, idx source.IndexInfo) []Statement {
	columns := quotedOscarColumns(d, idx.Columns)
	if idx.IsPrimary {
		return []Statement{{SQL: fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s)", d.QualifyTable(schema, idx.TableName), columns)}}
	}
	unique := ""
	if idx.IsUnique {
		unique = "UNIQUE "
	}
	return []Statement{{SQL: fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", unique, d.QuoteIdent(idx.IndexName), d.QualifyTable(schema, idx.TableName), columns)}}
}

func (d *OscarDialect) ForeignKeyStatements(schema string, fk source.FKInfo) []Statement {
	sql := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		d.QualifyTable(schema, fk.TableName), d.QuoteIdent(fk.ConstraintName), quotedOscarColumns(d, fk.Columns),
		d.QualifyTable(schema, fk.RefTable), quotedOscarColumns(d, fk.RefColumns))
	if fk.OnDelete != "" {
		sql += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		sql += " ON UPDATE " + fk.OnUpdate
	}
	return []Statement{{SQL: sql}}
}

func (d *OscarDialect) ViewStatements(schema string, view source.ViewInfo) []Statement {
	return []Statement{{SQL: "CREATE OR REPLACE VIEW " + d.QualifyTable(schema, view.ViewName) + " AS " + view.Definition}}
}

func (d *OscarDialect) CommentStatements(schema string, cm source.CommentInfo) []Statement {
	comment := strings.ReplaceAll(cm.Comment, "'", "''")
	if cm.ColumnName == "" {
		return []Statement{{SQL: fmt.Sprintf("COMMENT ON TABLE %s IS '%s'", d.QualifyTable(schema, cm.TableName), comment)}}
	}
	return []Statement{{SQL: fmt.Sprintf("COMMENT ON COLUMN %s.%s IS '%s'", d.QualifyTable(schema, cm.TableName), d.QuoteIdent(cm.ColumnName), comment)}}
}

func (d *OscarDialect) AdjustViewDefinition(definition string) string { return definition }
