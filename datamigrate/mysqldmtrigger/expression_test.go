package mysqldmtrigger

import (
	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
	"strings"
	"testing"
)

func expressionFixture() *converter {
	t := &source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Amount", DataType: "int"}, {Name: "Note", DataType: "varchar", Length: 80}}}
	p := parser.New()
	return &converter{info: source.TriggerInfo{Schema: "Src", TableName: "Orders", Timing: "BEFORE", Event: "UPDATE"}, tables: map[string]*source.TableDDLInfo{"Orders": t}, opt: Options{Schema: "DEST", ColumnNames: map[string]map[string]string{"Orders": {"Amount": "RenamedAmount"}}, CharacterMetadata: &source.TriggerCharacterMetadata{Columns: []source.TriggerColumnCharacter{{Schema: "Src", Table: "Orders", Name: "Note", Charset: "utf8mb4", Collation: "utf8mb4_bin"}}, NumericColumns: []source.TriggerNumericColumn{{Schema: "Src", Table: "Orders", Name: "Amount", ColumnType: "int"}}}}, parser: p, scopes: []map[string]variable{{"v": {name: "DBGOLD_VAR_1", kind: kindNumber}}}, deps: map[string]*source.TableDDLInfo{}}
}
func TestExpressionMappingsFunctionsAndNulls(t *testing.T) {
	c := expressionFixture()
	for _, tc := range []struct{ input, fragment string }{{"NEW.Amount+v", `:NEW."RENAMEDAMOUNT" + DBGOLD_VAR_1`}, {"CONCAT('x',NEW.Amount,1.2300)", "CASE WHEN"}, {"IFNULL(NEW.Amount,0)", "COALESCE"}, {"NOW(3)", "LOCALTIMESTAMP(3)"}, {"CASE WHEN NEW.Amount>0 THEN 1 ELSE 0 END", "CASE WHEN"}, {"NEW.Amount <=> NULL", "THEN 1"}, {"NEW.Note='hello'", "RAWTOHEX(UTL_RAW.CAST_TO_RAW(RTRIM"}, {"CHAR_LENGTH(NEW.Note)", "LENGTH("}, {"NULL IS FALSE", "ELSE -1"}} {
		v, err := c.expression(tc.input, nil)
		if err != nil {
			t.Errorf("%s: %v", tc.input, err)
			continue
		}
		if !strings.Contains(v.sql, tc.fragment) {
			t.Errorf("%s generated %s, missing %s", tc.input, v.sql, tc.fragment)
		}
	}
	c.opt.Capabilities.EmptyStringIsNull = true
	for _, s := range []string{"''", "CONCAT(NEW.Note,'x')", "SUBSTRING(NEW.Note,0)"} {
		if _, err := c.expression(s, nil); err == nil {
			t.Errorf("accepted null-sensitive %s", s)
		}
	}
}
func TestExpressionRejectsUnsafeOrUnknownAST(t *testing.T) {
	c := expressionFixture()
	for _, s := range []string{"(SELECT Amount FROM Orders)", "@session_variable", "unknown_fn(NEW.Amount)", "other.fn(1)", "NEW.Amount+'x'", "DISTINCT NEW.Amount", "NEW.Amount AS x", "LENGTH(NEW.Note)"} {
		if _, err := c.expression(s, nil); err == nil {
			t.Errorf("accepted unsupported expression: %s", s)
		}
	}
	c.opt.CharacterMetadata.Columns[0].Collation = "utf8mb4_general_ci"
	if _, err := c.expression("NEW.Note='x'", nil); err == nil {
		t.Fatal("accepted general_ci without validated normalization")
	}
}
func TestPredicatesPreserveNumericNullTruth(t *testing.T) {
	c := expressionFixture()
	for _, v := range []expression{{sql: "0", kind: kindNumber}, {sql: "NULL", kind: kindNull}} {
		s, err := c.predicate(v)
		if err != nil || s == "" {
			t.Fatalf("predicate %v: %s %v", v, s, err)
		}
	}
	if _, err := c.predicate(expression{sql: "'false'", kind: kindText}); err == nil {
		t.Fatal("accepted text coercion")
	}
}
