package mysqldmtrigger

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Only a complete SET value may use this non-deterministic expression. General
// expression conversion can repeat its operands (for example, CONCAT's NULL
// checks), which would generate different identifiers within one source call.
func (c *converter) assignmentExpression(s string) (expression, error) {
	n, err := c.parseExpression(s)
	if err != nil {
		return expression{}, err
	}
	if compactUUIDAssignment(n) {
		// TiDB erases FROM DUAL and treats explicit SQL_CACHE like its default.
		// Keep those outside this extension without changing legacy expressions.
		tokens, err := lex(s, c.mode.HasANSIQuotesMode(), c.mode.HasNoBackslashEscapesMode())
		if err != nil {
			return expression{}, err
		}
		for _, token := range tokens {
			if token.word == "FROM" || token.word == "SQL_CACHE" {
				return c.expr(n, nil)
			}
		}
		// Match the whole source operation rather than translating its empty
		// replacement string: Dameng may treat that string as NULL. SYS_GUID
		// is evaluated once, preserving the identifier format, not UUID v1 time.
		return expression{sql: "LOWER(RAWTOHEX(SYS_GUID()))", kind: kindText, table: "derived"}, nil
	}
	return c.expr(n, nil)
}

func compactUUIDAssignment(n ast.ExprNode) bool {
	for {
		n = withoutParentheses(n)
		subquery, ok := n.(*ast.SubqueryExpr)
		if !ok {
			break
		}
		if subquery.MultiRows || subquery.Exists {
			return false
		}
		query, ok := subquery.Query.(*ast.SelectStmt)
		if !ok {
			return false
		}
		var err error
		n, err = scalarSelectExpression(query)
		if err != nil {
			return false
		}
	}
	replace, ok := n.(*ast.FuncCallExpr)
	if !ok || replace.Schema.O != "" || !strings.EqualFold(replace.FnName.O, "replace") || len(replace.Args) != 3 {
		return false
	}
	uuid, ok := withoutParentheses(replace.Args[0]).(*ast.FuncCallExpr)
	return ok && uuid.Schema.O == "" && strings.EqualFold(uuid.FnName.O, "uuid") && len(uuid.Args) == 0 &&
		uuidStringLiteral(replace.Args[1], "-") && uuidStringLiteral(replace.Args[2], "")
}

func withoutParentheses(n ast.ExprNode) ast.ExprNode {
	for {
		p, ok := n.(*ast.ParenthesesExpr)
		if !ok {
			return n
		}
		n = p.Expr
	}
}

func uuidStringLiteral(n ast.ExprNode, want string) bool {
	v, ok := withoutParentheses(n).(ast.ValueExpr)
	if !ok {
		return false
	}
	s, ok := v.GetValue().(string)
	if !ok || s != want {
		return false
	}
	charset := v.GetType().GetCharset()
	return charset == "" || charset == "utf8" || charset == "utf8mb4"
}
