package dialect

import (
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/source"
)

func (d *OracleDialect) IndexName(i source.IndexInfo) string {
	if i.IsPrimary {
		return OracleObjectName("PK", i.TableName)
	}
	return OracleObjectName("IDX", i.TableName, i.IndexName)
}
func (d *OracleDialect) ForeignKeyName(f source.FKInfo) string {
	return OracleObjectName("FK", f.TableName, f.ConstraintName)
}
func (d *OracleDialect) SequenceName(s source.SequenceInfo) string {
	return OracleObjectName("SEQ", s.TableName, s.ColumnName)
}
func (d *OracleDialect) TriggerName(s source.SequenceInfo) string {
	return OracleObjectName("TRG", s.TableName, s.ColumnName)
}
func (d *OracleDialect) SequenceMarker(schema string, s source.SequenceInfo) string {
	return "DBGOLD_MYSQL_ORACLE:" + d.QualifyTable(schema, s.TableName) + "." + d.QuoteIdent(s.ColumnName)
}
func (d *OracleDialect) IndexStatements(schema string, i source.IndexInfo) []Statement {
	cols := make([]string, len(i.Columns))
	for n, c := range i.Columns {
		cols[n] = d.QuoteIdent(c)
	}
	name := d.QuoteIdent(d.IndexName(i))
	if i.IsPrimary {
		return []Statement{{SQL: "ALTER TABLE " + d.QualifyTable(schema, i.TableName) + " ADD CONSTRAINT " + name + " PRIMARY KEY (" + strings.Join(cols, ", ") + ")"}}
	}
	u := ""
	if i.IsUnique {
		u = "UNIQUE "
	}
	return []Statement{{SQL: "CREATE " + u + "INDEX " + d.QualifyTable(schema, d.IndexName(i)) + " ON " + d.QualifyTable(schema, i.TableName) + " (" + strings.Join(cols, ", ") + ")"}}
}
func ValidateOracleForeignKey(f source.FKInfo) error {
	if f.Unsupported != "" {
		return fmt.Errorf("%s", f.Unsupported)
	}
	if len(f.Columns) == 0 || len(f.Columns) != len(f.RefColumns) {
		return fmt.Errorf("外键列信息不完整")
	}
	switch strings.ToUpper(strings.TrimSpace(f.OnUpdate)) {
	case "", "RESTRICT", "NO ACTION":
	default:
		return fmt.Errorf("Oracle 不支持 ON UPDATE %s", f.OnUpdate)
	}
	switch strings.ToUpper(strings.TrimSpace(f.OnDelete)) {
	case "", "RESTRICT", "NO ACTION", "CASCADE", "SET NULL":
	default:
		return fmt.Errorf("Oracle 不支持 ON DELETE %s", f.OnDelete)
	}
	return nil
}
func (d *OracleDialect) ForeignKeyStatements(schema string, f source.FKInfo) []Statement {
	cols, refs := make([]string, len(f.Columns)), make([]string, len(f.RefColumns))
	for i, c := range f.Columns {
		cols[i] = d.QuoteIdent(c)
	}
	for i, c := range f.RefColumns {
		refs[i] = d.QuoteIdent(c)
	}
	s := "ALTER TABLE " + d.QualifyTable(schema, f.TableName) + " ADD CONSTRAINT " + d.QuoteIdent(d.ForeignKeyName(f)) + " FOREIGN KEY (" + strings.Join(cols, ", ") + ") REFERENCES " + d.QualifyTable(schema, f.RefTable) + " (" + strings.Join(refs, ", ") + ")"
	if action := strings.ToUpper(f.OnDelete); action == "CASCADE" || action == "SET NULL" {
		s += " ON DELETE " + action
	}
	return []Statement{{SQL: s}}
}

func OracleSequenceStart(s source.SequenceInfo) (string, error) {
	text := s.StartValueText
	if text == "" {
		text = fmt.Sprint(s.StartValue)
	}
	n, ok := new(big.Int).SetString(text, 10)
	if !ok || n.Sign() < 0 || len(n.String()) > 28 {
		return "", fmt.Errorf("序列 %s.%s 起始值超出 Oracle 范围", s.TableName, s.ColumnName)
	}
	if n.Sign() == 0 {
		return "1", nil
	}
	return n.String(), nil
}

func (d *OracleDialect) SequenceStatements(schema string, s source.SequenceInfo, _ string) []Statement {
	start, err := OracleSequenceStart(s)
	if err != nil {
		return nil
	}
	seq := d.QualifyTable(schema, d.SequenceName(s))
	table := d.QualifyTable(schema, s.TableName)
	// All SQL, including runtime MAX(ID), lives in the reported statement. The
	// preflight records provenance before DROP TABLE removes the old trigger.
	body := "DECLARE n NUMBER; old_n NUMBER := 1; BEGIN\n"
	if d.OwnedSequences[d.SequenceName(s)] {
		body += "SELECT LAST_NUMBER INTO old_n FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER = " + oracleLiteral(schema) + " AND SEQUENCE_NAME = " + oracleLiteral(d.SequenceName(s)) + ";\n"
	}
	body += "SELECT GREATEST(NVL(MAX(" + d.QuoteIdent(s.ColumnName) + "), 0) + 1, " + start + ", old_n) INTO n FROM " + table + ";\n"
	body += "IF n > 9999999999999999999999999999 THEN RAISE_APPLICATION_ERROR(-20001, 'Oracle sequence maximum exceeded'); END IF;\n"
	if d.OwnedSequences[d.SequenceName(s)] {
		body += "EXECUTE IMMEDIATE " + oracleLiteral("DROP SEQUENCE "+seq) + ";\n"
	}
	body += "EXECUTE IMMEDIATE " + oracleLiteral("CREATE SEQUENCE "+seq+" START WITH ") + " || TO_CHAR(n, 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''') || ' INCREMENT BY 1 NOMAXVALUE NOCYCLE CACHE 20';\nEND;"
	trigger := "CREATE OR REPLACE TRIGGER " + d.QualifyTable(schema, d.TriggerName(s)) + "\nBEFORE INSERT ON " + table + " FOR EACH ROW\nBEGIN\n  /* " + strings.ReplaceAll(d.SequenceMarker(schema, s), "*/", "* /") + " */\n  IF :NEW." + d.QuoteIdent(s.ColumnName) + " IS NULL THEN\n    SELECT " + seq + ".NEXTVAL INTO :NEW." + d.QuoteIdent(s.ColumnName) + " FROM DUAL;\n  END IF;\nEND;"
	return []Statement{{SQL: body}, {SQL: trigger}}
}
func (d *OracleDialect) ViewStatements(schema string, v source.ViewInfo) []Statement {
	columns := ""
	if len(v.Columns) > 0 {
		names := make([]string, len(v.Columns))
		for i, c := range v.Columns {
			names[i] = d.QuoteIdent(c.Name)
		}
		columns = " (" + strings.Join(names, ", ") + ")"
	}
	return []Statement{{SQL: "CREATE OR REPLACE VIEW " + d.QualifyTable(schema, v.ViewName) + columns + " AS " + v.Definition}}
}
func (d *OracleDialect) AdjustViewDefinition(s string) string { return s }
func (d *OracleDialect) CommentStatements(schema string, c source.CommentInfo) []Statement {
	object, kind := d.QualifyTable(schema, c.TableName), "TABLE"
	if c.ColumnName != "" {
		kind = "COLUMN"
		object += "." + d.QuoteIdent(c.ColumnName)
	}
	return []Statement{{SQL: "COMMENT ON " + kind + " " + object + " IS " + oracleLiteral(c.Comment)}}
}
