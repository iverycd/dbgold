package damengnative

import (
	"strings"
	"testing"
)

func TestRewriteSchemaPreservesProgramAndLiterals(t *testing.T) {
	sql := "CREATE PROCEDURE \"App\".\"p\" AS BEGIN\n-- App.x\nINSERT INTO \"App\".\"Mixed\" VALUES ('App.x', q'[App.x]', 'it''s;safe');\nEND;\n/"
	out, warnings, err := RewriteSchema(sql, "App", "Target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"Target"."Mixed"`) || !strings.Contains(out, `'it''s;safe'`) || !strings.Contains(out, "-- App.x") || !strings.HasSuffix(out, "END;") {
		t.Fatal(out)
	}
	if len(warnings) != 2 {
		t.Fatal(warnings)
	}
}

func TestRewriteSchemaQuotedCaseAndUnquotedFold(t *testing.T) {
	out, _, err := RewriteSchema(`SELECT APP.t, "App".t, "APP".t, MYAPP.t FROM APP.t`, "APP", "a\"b")
	if err != nil {
		t.Fatal(err)
	}
	if out != `SELECT "a""b".t, "App".t, "a""b".t, MYAPP.t FROM "a""b".t` {
		t.Fatal(out)
	}
}

func TestRewriteSchemaRejectsIncompleteSource(t *testing.T) {
	for _, sql := range []string{"SELECT 'bad", `SELECT "bad`, "/* bad", "SELECT q'[bad'"} {
		if _, _, err := RewriteSchema(sql, "A", "B"); err == nil {
			t.Fatalf("accepted %s", sql)
		}
	}
}

func TestOrderDependencyAndCycle(t *testing.T) {
	objects := []Object{{ID: "view", Kind: "view", Dependencies: []string{"table"}}, {ID: "table", Kind: "table"}, {ID: "body", Kind: "package_body", Dependencies: []string{"spec"}}, {ID: "spec", Kind: "package", Dependencies: []string{"body"}}}
	got := Order(objects)
	position := map[string]int{}
	for i, o := range got {
		position[o.ID] = i
	}
	if position["table"] > position["view"] || position["spec"] > position["body"] {
		t.Fatal(got)
	}
}

func TestRewriteSchemaBlocksShadowingInsteadOfRewritingAliases(t *testing.T) {
	for _, sql := range []string{`CREATE VIEW APP.v AS SELECT APP.id FROM APP.t APP`, `CREATE VIEW APP.v AS WITH APP AS (SELECT id FROM t) SELECT APP.id FROM APP`, `CREATE PROCEDURE APP.p(APP IN NUMBER) AS BEGIN x:=APP.value; END;`} {
		_, _, err := RewriteSchema(sql, "APP", "DST")
		if err == nil {
			t.Fatalf("accepted ambiguous schema mapping: %s", sql)
		}
	}
}
