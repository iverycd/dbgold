package tsqldameng

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

var viewNumber = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)

type token struct {
	text  string
	ident bool
}

// tokenize only normalizes identifier quotes. String contents (including backslashes,
// doubled quotes and Unicode) are never rewritten. Unsupported T-SQL is rejected.
func tokenize(s string) ([]token, error) {
	out := []token{}
	for i := 0; i < len(s); {
		if s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "--") {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			return nil, fmt.Errorf("视图暂不支持块注释")
		}
		if (s[i] == 'N' || s[i] == 'n') && i+1 < len(s) && s[i+1] == '\'' {
			i++
		}
		if s[i] == '\'' {
			end, e := quotedEnd(s, i, '\'')
			if e != nil {
				return nil, e
			}
			out = append(out, token{s[i : end+1], false})
			i = end + 1
			continue
		}
		if s[i] == '[' || s[i] == '"' {
			q := s[i]
			endq := q
			if q == '[' {
				endq = ']'
			}
			j := i + 1
			var name strings.Builder
			for ; j < len(s); j++ {
				if s[j] == endq {
					if j+1 < len(s) && s[j+1] == endq {
						name.WriteByte(endq)
						j++
						continue
					}
					break
				}
				name.WriteByte(s[j])
			}
			if j == len(s) {
				return nil, fmt.Errorf("视图标识符未闭合")
			}
			out = append(out, token{name.String(), true})
			i = j + 1
			continue
		}
		if (s[i] >= '0' && s[i] <= '9') || (s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9') {
			literal := viewNumber.FindString(s[i:])
			out = append(out, token{literal, false})
			i += len(literal)
			continue
		}
		if strings.ContainsRune("()+-*/%,.=<>!;", rune(s[i])) {
			out = append(out, token{s[i : i+1], false})
			i++
			continue
		}
		start := i
		for i < len(s) && !strings.ContainsRune(" \r\n\t[]\"'()+-*/%,.=<>!;", rune(s[i])) {
			i++
		}
		if i == start {
			return nil, fmt.Errorf("不支持的视图字符")
		}
		text := s[start:i]
		if strings.ContainsAny(text, "@:#`\\") {
			return nil, fmt.Errorf("不支持视图标记 %q", text)
		}
		out = append(out, token{text, false})
	}
	return out, nil
}
func keyword(t token, s string) bool { return !t.ident && strings.EqualFold(t.text, s) }
func q(s string) string              { return `"` + strings.ReplaceAll(strings.ToUpper(s), `"`, `""`) + `"` }

// View supports SELECT projections, joins, predicates, grouping and a small
// aggregate whitelist. Unknown AST nodes fail closed rather than changing meaning.
func View(raw, targetSchema string) (string, []string, error) {
	ts, err := tokenize(raw)
	if err != nil {
		return "", nil, err
	}
	if len(ts) < 4 {
		return "", nil, fmt.Errorf("视图定义为空或不完整（可能已加密）")
	}
	i := 0
	if keyword(ts[i], "CREATE") {
		i++
		if i+1 < len(ts) && keyword(ts[i], "OR") && keyword(ts[i+1], "ALTER") {
			i += 2
		}
	} else if keyword(ts[i], "ALTER") {
		i++
	} else {
		return "", nil, fmt.Errorf("视图缺少 CREATE/ALTER VIEW 头")
	}
	if i >= len(ts) || !keyword(ts[i], "VIEW") {
		return "", nil, fmt.Errorf("不支持的视图头")
	}
	i++
	// One/two-part view name. Column lists and WITH SCHEMABINDING fail closed.
	i++
	if i < len(ts) && ts[i].text == "." {
		i += 2
	}
	if i >= len(ts) || !keyword(ts[i], "AS") {
		return "", nil, fmt.Errorf("暂不支持视图列清单或 WITH 选项")
	}
	i++
	parts := []string{}
	for _, t := range ts[i:] {
		if t.ident {
			parts = append(parts, "`"+strings.ReplaceAll(t.text, "`", "``")+"`")
			continue
		}
		switch strings.ToUpper(t.text) {
		case "TOP", "WITH", "APPLY", "PIVOT", "UNPIVOT", "COLLATE", "OVER", "CONVERT", "TRY_CONVERT", "CAST", "TRY_CAST", "ORDER", "LIMIT", "OFFSET", "INTO", "+", "-", "/", "%":
			return "", nil, fmt.Errorf("暂不支持视图语法 %s", t.text)
		}
		parts = append(parts, t.text)
	}
	p := parser.New()
	p.SetSQLMode(mysql.ModeNoBackslashEscapes | mysql.ModeIgnoreSpace)
	n, err := p.ParseOneStmt(strings.Join(parts, " "), "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return "", nil, fmt.Errorf("视图 SELECT 解析失败: %w", err)
	}
	r := &viewRenderer{schema: targetSchema, deps: map[string]bool{}}
	sql := r.selectNode(n)
	deps := []string{}
	for d := range r.deps {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return sql, deps, r.err
}

type viewRenderer struct {
	schema string
	deps   map[string]bool
	err    error
}

func (r *viewRenderer) fail(s string) string {
	if r.err == nil {
		r.err = fmt.Errorf("视图暂不支持 %s", s)
	}
	return ""
}
func (r *viewRenderer) selectNode(n ast.Node) string {
	s, ok := n.(*ast.SelectStmt)
	if !ok {
		return r.fail("UNION 或非 SELECT 语句")
	}
	if s.With != nil || s.Limit != nil || s.OrderBy != nil || s.SelectIntoOpt != nil || s.LockInfo != nil || len(s.WindowSpecs) > 0 {
		return r.fail("WITH、排序、窗口、锁定或行数限制")
	}
	fields := []string{}
	for _, f := range s.Fields.Fields {
		v := ""
		if f.WildCard != nil {
			v = "*"
			if f.WildCard.Schema.O != "" {
				return r.fail("三段式通配列")
			}
			if f.WildCard.Table.O != "" {
				v = q(f.WildCard.Table.O) + ".*"
			}
		} else {
			v = r.expr(f.Expr)
		}
		if f.AsName.O != "" {
			v += " AS " + q(f.AsName.O)
		}
		fields = append(fields, v)
	}
	sql := "SELECT "
	if s.Distinct {
		sql += "DISTINCT "
	}
	sql += strings.Join(fields, ", ")
	if s.From != nil {
		sql += " FROM " + r.relation(s.From.TableRefs)
	}
	if s.Where != nil {
		sql += " WHERE " + r.expr(s.Where)
	}
	if s.GroupBy != nil {
		if s.GroupBy.Rollup {
			return r.fail("ROLLUP")
		}
		group := []string{}
		for _, g := range s.GroupBy.Items {
			group = append(group, r.expr(g.Expr))
		}
		sql += " GROUP BY " + strings.Join(group, ", ")
	}
	if s.Having != nil {
		sql += " HAVING " + r.expr(s.Having.Expr)
	}
	return sql
}
func (r *viewRenderer) relation(n ast.ResultSetNode) string {
	switch n := n.(type) {
	case *ast.TableName:
		if n.Schema.O != "" && !strings.EqualFold(n.Schema.O, "dbo") {
			return r.fail("跨 schema 引用 " + n.Schema.O)
		}
		r.deps[n.Name.O] = true
		if r.schema == "" {
			return q(n.Name.O)
		}
		return q(r.schema) + "." + q(n.Name.O)
	case *ast.TableSource:
		sql := r.relation(n.Source)
		if n.AsName.O != "" {
			sql += " " + q(n.AsName.O)
		}
		return sql
	case *ast.Join:
		left := r.relation(n.Left)
		if n.Right == nil {
			return left
		}
		if n.NaturalJoin || len(n.Using) > 0 || n.StraightJoin {
			return r.fail("NATURAL/USING/STRAIGHT JOIN")
		}
		join := " CROSS JOIN "
		if n.On != nil {
			join = " INNER JOIN "
		}
		if n.Tp == ast.LeftJoin {
			join = " LEFT JOIN "
		}
		if n.Tp == ast.RightJoin {
			join = " RIGHT JOIN "
		}
		sql := left + join + r.relation(n.Right)
		if n.On != nil {
			sql += " ON " + r.expr(n.On.Expr)
		}
		return sql
	default:
		return r.fail("子查询表源")
	}
}
func (r *viewRenderer) expr(n ast.ExprNode) string {
	switch n := n.(type) {
	case *ast.ColumnNameExpr:
		if n.Name.Schema.O != "" {
			return r.fail("三段式列引用")
		}
		s := q(n.Name.Name.O)
		if n.Name.Table.O != "" {
			s = q(n.Name.Table.O) + "." + s
		}
		return s
	case ast.ValueExpr:
		v := n.GetValue()
		if v == nil {
			return "NULL"
		}
		switch v := v.(type) {
		case string:
			return "'" + strings.ReplaceAll(v, "'", "''") + "'"
		case []byte:
			return r.fail("二进制字面量")
		default:
			s := fmt.Sprint(v)
			if !number.MatchString(s) {
				return r.fail("字面量 " + s)
			}
			return s
		}
	case *ast.ParenthesesExpr:
		return "(" + r.expr(n.Expr) + ")"
	case *ast.BinaryOperationExpr:
		ops := map[opcode.Op]string{opcode.EQ: "=", opcode.NE: "<>", opcode.LT: "<", opcode.LE: "<=", opcode.GT: ">", opcode.GE: ">=", opcode.LogicAnd: "AND", opcode.LogicOr: "OR"}
		op, ok := ops[n.Op]
		if !ok {
			return r.fail("算术/位运算")
		}
		return "(" + r.expr(n.L) + " " + op + " " + r.expr(n.R) + ")"
	case *ast.UnaryOperationExpr:
		if n.Op != opcode.Not {
			return r.fail("一元运算")
		}
		return "NOT (" + r.expr(n.V) + ")"
	case *ast.IsNullExpr:
		s := r.expr(n.Expr) + " IS "
		if n.Not {
			s += "NOT "
		}
		return s + "NULL"
	case *ast.AggregateFuncExpr:
		name := strings.ToUpper(n.F)
		switch name {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
		default:
			return r.fail("聚合函数 " + name)
		}
		args := []string{}
		for _, a := range n.Args {
			args = append(args, r.expr(a))
		}
		distinct := ""
		if n.Distinct {
			distinct = "DISTINCT "
		}
		return name + "(" + distinct + strings.Join(args, ", ") + ")"
	case *ast.FuncCallExpr:
		return r.fail("函数 " + n.FnName.O)
	default:
		return r.fail(fmt.Sprintf("表达式 %T", n))
	}
}

// ValidName matches DM8's byte limit after the chosen uppercase normalization.
func ValidName(s string) error {
	if s == "" || len(strings.ToUpper(s)) > 128 {
		return fmt.Errorf("对象名为空或超过达梦 128 字节限制: %q", s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("对象名含控制字符: %q", s)
		}
	}
	return nil
}
