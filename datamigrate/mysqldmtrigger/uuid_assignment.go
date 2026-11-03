package mysqldmtrigger

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Only a complete SET value may use this non-deterministic expression. General
// expression conversion can repeat its operands (for example, CONCAT's NULL
// checks), which would generate different identifiers within one source call.
// The optional variable holds the single generated UUID before its five fields
// are formatted; setStatement scopes it around just this assignment.
func (c *converter) assignmentExpression(s string) (expression, string, error) {
	n, err := c.parseExpression(s)
	if err != nil {
		return expression{}, "", err
	}
	compact, bare := compactUUIDAssignment(n), bareUUIDAssignment(n)
	if compact || bare {
		// TiDB erases FROM DUAL and treats explicit SQL_CACHE like its default.
		// Keep those outside this extension without changing legacy expressions.
		tokens, err := lex(s, c.mode.HasANSIQuotesMode(), c.mode.HasNoBackslashEscapesMode())
		if err != nil {
			return expression{}, "", err
		}
		for _, token := range tokens {
			if token.word == "FROM" || token.word == "SQL_CACHE" {
				v, err := c.expr(n, nil)
				return v, "", err
			}
		}
		// Match the whole source operation rather than translating its empty
		// replacement string: Dameng may treat that string as NULL. SYS_GUID
		// is evaluated once, preserving the identifier format, not UUID v1 time.
		if compact {
			return expression{sql: "LOWER(RAWTOHEX(SYS_GUID()))", kind: kindText, table: "derived"}, "", nil
		}
		variable := c.fresh("UUID")
		name := quote(variable)
		parts := []string{
			"SUBSTR(" + name + ",1,8)",
			"SUBSTR(" + name + ",9,4)",
			"SUBSTR(" + name + ",13,4)",
			"SUBSTR(" + name + ",17,4)",
			"SUBSTR(" + name + ",21,12)",
		}
		return expression{sql: strings.Join(parts, " || '-' || "), kind: kindText, table: "derived"}, variable, nil
	}
	v, err := c.expr(n, nil)
	return v, "", err
}

func compactUUIDAssignment(n ast.ExprNode) bool {
	n = unwrappedUUIDAssignment(n)
	replace, ok := n.(*ast.FuncCallExpr)
	if !ok || replace.Schema.O != "" || !strings.EqualFold(replace.FnName.O, "replace") || len(replace.Args) != 3 {
		return false
	}
	return isUUIDCall(withoutParentheses(replace.Args[0])) &&
		uuidStringLiteral(replace.Args[1], "-") && uuidStringLiteral(replace.Args[2], "")
}

func bareUUIDAssignment(n ast.ExprNode) bool {
	return isUUIDCall(unwrappedUUIDAssignment(n))
}

func isUUIDCall(n ast.ExprNode) bool {
	uuid, ok := n.(*ast.FuncCallExpr)
	return ok && uuid.Schema.O == "" && strings.EqualFold(uuid.FnName.O, "uuid") && len(uuid.Args) == 0
}

func unwrappedUUIDAssignment(n ast.ExprNode) ast.ExprNode {
	for {
		n = withoutParentheses(n)
		subquery, ok := n.(*ast.SubqueryExpr)
		if !ok {
			return n
		}
		if subquery.MultiRows || subquery.Exists {
			return nil
		}
		query, ok := subquery.Query.(*ast.SelectStmt)
		if !ok {
			return nil
		}
		var err error
		n, err = scalarSelectExpression(query)
		if err != nil {
			return nil
		}
	}
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
