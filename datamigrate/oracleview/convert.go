// Package oracleview translates an explicit MySQL SELECT subset to Oracle 11g.
// Every AST node is handled explicitly: unknown syntax is never passed through.
package oracleview

import (
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate/dialect"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

type renderer struct {
	source, target string
	strip          map[string]bool
	deps           map[string]bool
	err            error
}

func Convert(sql, sourceSchema, targetSchema string, stripSchemas []string) (string, []string, error) {
	n, err := parser.New().ParseOneStmt(sql, "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return "", nil, fmt.Errorf("解析 MySQL 视图: %w", err)
	}
	r := &renderer{source: sourceSchema, target: targetSchema, strip: map[string]bool{}, deps: map[string]bool{}}
	for _, s := range stripSchemas {
		r.strip[strings.ToUpper(s)] = true
	}
	converted := r.selectNode(n)
	deps := []string{}
	for name := range r.deps {
		deps = append(deps, name)
	}
	sort.Strings(deps)
	return converted, deps, r.err
}
func (r *renderer) fail(format string, args ...interface{}) string {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
	return ""
}
func (r *renderer) ident(s string) string {
	s = strings.ToUpper(s)
	if err := dialect.ValidateOracleName(s); err != nil {
		return r.fail("%v", err)
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
func (r *renderer) schema(s string) string {
	if s != "" && s != r.source && !r.strip[strings.ToUpper(s)] {
		return r.fail("不支持视图跨 database 引用 %s", s)
	}
	return `"` + strings.ReplaceAll(r.target, `"`, `""`) + `"`
}
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (r *renderer) selectNode(n ast.Node) string {
	switch n := n.(type) {
	case *ast.SelectStmt:
		if n.Kind != ast.SelectStmtKindSelect || n.With != nil || n.Limit != nil || n.LockInfo != nil || n.SelectIntoOpt != nil || len(n.WindowSpecs) > 0 {
			return r.fail("Oracle 视图暂不支持 WITH、LIMIT、窗口、锁定或非 SELECT 语法")
		}
		if n.SelectStmtOpts != nil && (n.CalcFoundRows || n.StraightJoin) {
			return r.fail("不支持 SQL_CALC_FOUND_ROWS/STRAIGHT_JOIN")
		}
		fields := []string{}
		if n.Fields == nil {
			return r.fail("SELECT 缺少列")
		}
		for _, f := range n.Fields.Fields {
			s := ""
			if f.WildCard != nil {
				if f.WildCard.Schema.O != "" {
					r.schema(f.WildCard.Schema.O)
				}
				s = "*"
				if f.WildCard.Table.O != "" {
					s = r.ident(f.WildCard.Table.O) + ".*"
				}
			} else {
				s = r.expr(f.Expr, false)
			}
			if f.AsName.O != "" {
				s += " AS " + r.ident(f.AsName.O)
			}
			fields = append(fields, s)
		}
		s := "SELECT "
		if n.Distinct {
			s += "DISTINCT "
		}
		s += strings.Join(fields, ", ")
		if n.From != nil {
			s += " FROM " + r.from(n.From.TableRefs)
		} else {
			s += " FROM DUAL"
		}
		if n.Where != nil {
			s += " WHERE " + r.expr(n.Where, true)
		}
		if n.GroupBy != nil {
			if n.GroupBy.Rollup {
				return r.fail("暂不支持 WITH ROLLUP")
			}
			items := []string{}
			for _, i := range n.GroupBy.Items {
				if _, ok := i.Expr.(*ast.PositionExpr); ok {
					return r.fail("GROUP BY 序号需要手动改为列表达式")
				}
				items = append(items, r.expr(i.Expr, false))
			}
			s += " GROUP BY " + strings.Join(items, ", ")
		}
		if n.Having != nil {
			s += " HAVING " + r.expr(n.Having.Expr, true)
		}
		return s + r.order(n.OrderBy)
	case *ast.SetOprStmt:
		if n.Limit != nil || n.With != nil {
			return r.fail("UNION 暂不支持 LIMIT/WITH")
		}
		return r.selectNode(n.SelectList) + r.order(n.OrderBy)
	case *ast.SetOprSelectList:
		if n.Limit != nil || n.With != nil {
			return r.fail("UNION 暂不支持 LIMIT/WITH")
		}
		parts := []string{}
		for i, child := range n.Selects {
			var op *ast.SetOprType
			switch c := child.(type) {
			case *ast.SelectStmt:
				op = c.AfterSetOperator
				if c.OrderBy != nil {
					return r.fail("暂不支持 UNION 分支 ORDER BY")
				}
			case *ast.SetOprSelectList:
				return r.fail("暂不支持嵌套集合运算")
			default:
				return r.fail("不支持的 UNION 分支 %T", child)
			}
			if i > 0 {
				if op == nil || (*op != ast.Union && *op != ast.UnionAll) {
					return r.fail("仅支持 UNION/UNION ALL")
				}
				if *op == ast.UnionAll {
					parts = append(parts, "UNION ALL")
				} else {
					parts = append(parts, "UNION")
				}
			}
			parts = append(parts, r.selectNode(child))
		}
		return strings.Join(parts, " ") + r.order(n.OrderBy)
	default:
		return r.fail("仅支持 SELECT 视图，收到 %T", n)
	}
}
func (r *renderer) order(o *ast.OrderByClause) string {
	if o == nil {
		return ""
	}
	out := []string{}
	for _, i := range o.Items {
		s := r.expr(i.Expr, false)
		if i.Desc {
			s += " DESC NULLS LAST"
		} else {
			s += " ASC NULLS FIRST"
		}
		out = append(out, s)
	}
	return " ORDER BY " + strings.Join(out, ", ")
}
func (r *renderer) from(n ast.ResultSetNode) string {
	switch n := n.(type) {
	case *ast.Join:
		if n.NaturalJoin || n.StraightJoin || len(n.Using) > 0 {
			return r.fail("暂不支持 NATURAL/STRAIGHT JOIN 或 USING")
		}
		left := r.from(n.Left)
		if n.Right == nil {
			return left
		}
		join := " CROSS JOIN "
		if n.Tp == ast.LeftJoin {
			join = " LEFT JOIN "
		} else if n.Tp == ast.RightJoin {
			join = " RIGHT JOIN "
		} else if n.On != nil {
			join = " INNER JOIN "
		}
		s := left + join + r.from(n.Right)
		if n.On != nil {
			s += " ON " + r.expr(n.On.Expr, true)
		}
		return "(" + s + ")"
	case *ast.TableSource:
		s := ""
		switch n.Source.(type) {
		case *ast.SelectStmt, *ast.SetOprStmt:
			s = "(" + r.selectNode(n.Source) + ")"
		default:
			s = r.from(n.Source)
		}
		if n.AsName.O != "" {
			s += " " + r.ident(n.AsName.O)
		}
		return s
	case *ast.TableName:
		if len(n.PartitionNames) > 0 || len(n.IndexHints) > 0 {
			return r.fail("暂不支持分区限定或索引提示")
		}
		r.deps[n.Name.O] = true
		return r.schema(n.Schema.O) + "." + r.ident(n.Name.O)
	default:
		return r.fail("不支持的 FROM 节点 %T", n)
	}
}

func booleanExpr(e ast.ExprNode) bool {
	switch n := e.(type) {
	case *ast.ParenthesesExpr:
		return booleanExpr(n.Expr)
	case *ast.BinaryOperationExpr:
		switch n.Op {
		case opcode.EQ, opcode.NE, opcode.GT, opcode.GE, opcode.LT, opcode.LE, opcode.LogicAnd, opcode.LogicOr:
			return true
		}
	case *ast.UnaryOperationExpr:
		return n.Op == opcode.Not || n.Op == opcode.Not2
	case *ast.IsNullExpr, *ast.IsTruthExpr, *ast.BetweenExpr, *ast.PatternInExpr, *ast.PatternLikeOrIlikeExpr, *ast.ExistsSubqueryExpr:
		return true
	}
	return false
}
func (r *renderer) expr(e ast.ExprNode, predicate bool) string {
	s := r.rawExpr(e)
	if predicate && !booleanExpr(e) {
		return "(" + s + " <> 0)"
	}
	if !predicate && booleanExpr(e) {
		return "(CASE WHEN " + s + " THEN 1 WHEN NOT (" + s + ") THEN 0 ELSE NULL END)"
	}
	return s
}
func (r *renderer) rawExpr(e ast.ExprNode) string {
	switch n := e.(type) {
	case ast.ValueExpr:
		switch v := n.GetValue().(type) {
		case nil:
			return "NULL"
		case string:
			return literal(v)
		case int64, uint64, int, uint, float32, float64:
			return fmt.Sprint(v)
		default:
			if s, ok := v.(fmt.Stringer); ok {
				text := s.String()
				for _, c := range text {
					if !strings.ContainsRune("0123456789.eE+-", c) {
						return r.fail("不支持的视图字面量 %T", v)
					}
				}
				return text
			}
			return r.fail("不支持的视图字面量 %T", v)
		}
	case *ast.ColumnNameExpr:
		p := []string{}
		if n.Name.Schema.O != "" {
			p = append(p, r.schema(n.Name.Schema.O))
		}
		if n.Name.Table.O != "" {
			p = append(p, r.ident(n.Name.Table.O))
		}
		p = append(p, r.ident(n.Name.Name.O))
		return strings.Join(p, ".")
	case *ast.ParenthesesExpr:
		return "(" + r.rawExpr(n.Expr) + ")"
	case *ast.PositionExpr:
		return fmt.Sprint(n.N)
	case *ast.BinaryOperationExpr:
		ops := map[opcode.Op]string{opcode.EQ: "=", opcode.NE: "<>", opcode.GE: ">=", opcode.LE: "<=", opcode.GT: ">", opcode.LT: "<", opcode.Plus: "+", opcode.Minus: "-", opcode.Mul: "*", opcode.Div: "/", opcode.LogicAnd: "AND", opcode.LogicOr: "OR"}
		op, ok := ops[n.Op]
		if !ok {
			return r.fail("不支持运算符 %s", n.Op.String())
		}
		pred := n.Op == opcode.LogicAnd || n.Op == opcode.LogicOr
		return "(" + r.expr(n.L, pred) + " " + op + " " + r.expr(n.R, pred) + ")"
	case *ast.UnaryOperationExpr:
		switch n.Op {
		case opcode.Not, opcode.Not2:
			return "NOT (" + r.expr(n.V, true) + ")"
		case opcode.Plus:
			return "+(" + r.expr(n.V, false) + ")"
		case opcode.Minus:
			return "-(" + r.expr(n.V, false) + ")"
		default:
			return r.fail("不支持一元运算符 %s", n.Op.String())
		}
	case *ast.IsNullExpr:
		s := r.expr(n.Expr, false) + " IS "
		if n.Not {
			s += "NOT "
		}
		return s + "NULL"
	case *ast.IsTruthExpr:
		v := r.expr(n.Expr, false)
		condition := v + " <> 0"
		if n.True == 0 {
			condition = v + " = 0"
		}
		if n.Not {
			return "(" + v + " IS NULL OR NOT (" + condition + "))"
		}
		return "(" + condition + ")"
	case *ast.BetweenExpr:
		not := ""
		if n.Not {
			not = " NOT"
		}
		return "(" + r.expr(n.Expr, false) + not + " BETWEEN " + r.expr(n.Left, false) + " AND " + r.expr(n.Right, false) + ")"
	case *ast.PatternInExpr:
		not := ""
		if n.Not {
			not = " NOT"
		}
		list := []string{}
		for _, e := range n.List {
			list = append(list, r.expr(e, false))
		}
		if len(list) > 1000 {
			return r.fail("Oracle 11g IN 列表最多 1000 项")
		}
		rhs := strings.Join(list, ", ")
		if n.Sel != nil {
			rhs = r.expr(n.Sel, false)
			rhs = strings.TrimPrefix(strings.TrimSuffix(rhs, ")"), "(")
		}
		return "(" + r.expr(n.Expr, false) + not + " IN (" + rhs + "))"
	case *ast.PatternLikeOrIlikeExpr:
		if n.IsLike == false {
			return r.fail("暂不支持 ILIKE")
		}
		not := ""
		if n.Not {
			not = " NOT"
		}
		return "(" + r.expr(n.Expr, false) + not + " LIKE " + r.expr(n.Pattern, false) + " ESCAPE " + literal(string(n.Escape)) + ")"
	case *ast.SubqueryExpr:
		return "(" + r.selectNode(n.Query) + ")"
	case *ast.ExistsSubqueryExpr:
		not := ""
		if n.Not {
			not = "NOT "
		}
		return not + "EXISTS " + r.expr(n.Sel, false)
	case *ast.CaseExpr:
		s := "CASE"
		if n.Value != nil {
			s += " " + r.expr(n.Value, false)
		}
		for _, w := range n.WhenClauses {
			s += " WHEN " + r.expr(w.Expr, n.Value == nil) + " THEN " + r.expr(w.Result, false)
		}
		if n.ElseClause != nil {
			s += " ELSE " + r.expr(n.ElseClause, false)
		}
		return s + " END"
	case *ast.FuncCallExpr:
		name := strings.ToUpper(n.FnName.O)
		if n.Schema.O != "" {
			return r.fail("不支持自定义函数 %s.%s", n.Schema.O, name)
		}
		if name == "IF" {
			if len(n.Args) != 3 {
				return r.fail("IF 参数数量错误")
			}
			return "CASE WHEN " + r.expr(n.Args[0], true) + " THEN " + r.expr(n.Args[1], false) + " ELSE " + r.expr(n.Args[2], false) + " END"
		}
		if name == "NOW" || name == "CURRENT_TIMESTAMP" {
			if len(n.Args) != 0 {
				return r.fail("暂不支持带精度参数的 NOW/CURRENT_TIMESTAMP")
			}
			return "CURRENT_TIMESTAMP"
		}
		if name == "IFNULL" {
			if len(n.Args) != 2 {
				return r.fail("IFNULL 参数数量错误")
			}
			name = "COALESCE"
		}
		switch name {
		case "COALESCE", "NULLIF", "ABS", "ROUND", "LOWER", "UPPER":
		default:
			return r.fail("暂不支持 MySQL 函数 %s", name)
		}
		args := []string{}
		for _, a := range n.Args {
			args = append(args, r.expr(a, false))
		}
		return name + "(" + strings.Join(args, ", ") + ")"
	case *ast.AggregateFuncExpr:
		name := strings.ToUpper(n.F)
		switch name {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
		default:
			return r.fail("暂不支持聚合函数 %s", name)
		}
		if n.Order != nil || len(n.Args) != 1 {
			return r.fail("不支持该聚合参数/排序")
		}
		args := r.expr(n.Args[0], false)
		if n.Distinct {
			args = "DISTINCT " + args
		}
		return name + "(" + args + ")"
	default:
		return r.fail("不支持的 MySQL 表达式 %T", e)
	}
}
