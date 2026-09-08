package sqlsyntax

import (
	"strings"
	"testing"
)

func TestLiteralBoundaries(t *testing.T) {
	for _, raw := range []string{`'a::b'::text`, `'a''b'::text`, `E'a\\b'::text`, `''::varchar`} {
		ts, e := Lex(raw)
		if e != nil {
			t.Fatal(e)
		}
		p := Expr{Tokens: ts}
		out, e := p.Parse()
		if e != nil {
			t.Fatalf("%s: %v", raw, e)
		}
		if raw == `'a::b'::text` && out != `'a::b'` {
			t.Fatalf("cast inside literal damaged: %s", out)
		}
	}
}
func TestRejectUnsupportedExpressions(t *testing.T) {
	for _, raw := range []string{`gen_random_uuid()`, `x::integer`, `x + 1`, `'x'::varchar(1)`, `x IN (1)`, `x = 1; DROP TABLE x`, `EXISTS (SELECT 1)`} {
		ts, e := Lex(raw)
		if e == nil {
			p := Expr{Tokens: ts, Resolve: func(n []string) (string, error) { return strings.Join(n, "."), nil }}
			_, e = p.Parse()
		}
		if e == nil {
			t.Fatalf("accepted unsafe grammar: %s", raw)
		}
	}
}
func TestBooleanPrecedence(t *testing.T) {
	ts, _ := Lex(`NOT a = 1 AND b IS NOT NULL OR c = 'x'::text`)
	p := Expr{Tokens: ts, Resolve: func(n []string) (string, error) { return n[0], nil }}
	s, e := p.Parse()
	if e != nil {
		t.Fatal(e)
	}
	if s != `(((NOT (a = 1)) AND (b IS NOT NULL)) OR (c = 'x'))` {
		t.Fatal(s)
	}
}
