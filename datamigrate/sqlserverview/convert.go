// Package sqlserverview translates an explicit MySQL SELECT subset to SQL Server 2008 R2.
// Every AST node is handled explicitly: unknown syntax is never passed through.
package sqlserverview

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

type renderer struct {
	divIncrement   int
	source, target string
	strip          map[string]bool
	deps           map[string]bool
	err            error
	name           dialect.NameFunc
	tables         map[string]*source.TableDDLInfo
	aliases        map[string]string
	sources        map[string]bool
}

func Convert(sql, sourceSchema, targetSchema string, stripSchemas []string, name dialect.NameFunc, tables map[string]*source.TableDDLInfo, divisionPrecision ...int) (string, []string, error) {
	n, err := parser.New().ParseOneStmt(sql, "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return "", nil, fmt.Errorf("解析 MySQL 视图: %w", err)
	}
	inc := 4
	if len(divisionPrecision) > 0 {
		inc = divisionPrecision[0]
	}
	if inc < 0 || inc > 30 {
		return "", nil, fmt.Errorf("MySQL div_precision_increment 超出支持范围")
	}
	r := &renderer{divIncrement: inc, source: sourceSchema, target: targetSchema, strip: map[string]bool{}, deps: map[string]bool{}, name: name, tables: tables, aliases: map[string]string{}, sources: map[string]bool{}}
	for _, s := range stripSchemas {
		r.strip[strings.ToUpper(s)] = true
	}
	n.Accept(&aliasVisitor{r: r})
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
	s = r.name(s)
	if err := dialect.ValidateSQLServerName(s); err != nil {
		return r.fail("%v", err)
	}
	return dialect.NewSQLServer().QuoteIdent(s)
}
func (r *renderer) schema(s string) string {
	if s != "" && s != r.source && !r.strip[strings.ToUpper(s)] {
		return r.fail("不支持视图跨 database 引用 %s", s)
	}
	return dialect.NewSQLServer().QuoteIdent(r.target)
}
func literal(s string) string { return dialect.SQLServerLiteral(s) }

func (r *renderer) selectNode(n ast.Node) string {
	switch n := n.(type) {
	case *ast.SelectStmt:
		if n.Kind != ast.SelectStmtKindSelect || n.With != nil || n.LockInfo != nil || n.SelectIntoOpt != nil || len(n.WindowSpecs) > 0 {
			return r.fail("SQL Server 2008 R2 视图暂不支持 WITH、窗口、锁定或非 SELECT 语法")
		}
		if n.SelectStmtOpts != nil && (n.CalcFoundRows || n.StraightJoin) {
			return r.fail("不支持 SQL_CALC_FOUND_ROWS/STRAIGHT_JOIN")
		}
		if n.OrderBy != nil && n.Limit == nil {
			return r.fail("SQL Server 视图 ORDER BY 必须配合 LIMIT/TOP")
		}
		top := ""
		if n.Limit != nil {
			if n.Limit.Offset != nil {
				off, ok := uintLiteral(n.Limit.Offset)
				if !ok || off != 0 {
					return r.fail("SQL Server 2008 R2 暂不支持非零 LIMIT 偏移")
				}
			}
			count, ok := uintLiteral(n.Limit.Count)
			if !ok {
				return r.fail("LIMIT 必须为非负整数常量")
			}
			top = fmt.Sprintf("TOP (%d) ", count)
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
		s += top + strings.Join(fields, ", ")
		if n.From != nil {
			s += " FROM " + r.from(n.From.TableRefs)

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
		if n.Limit != nil || n.With != nil || n.OrderBy != nil {
			return r.fail("UNION 暂不支持 LIMIT/WITH")
		}
		return r.selectNode(n.SelectList) + r.order(n.OrderBy)
	case *ast.SetOprSelectList:
		if n.Limit != nil || n.With != nil || n.OrderBy != nil {
			return r.fail("UNION 暂不支持 LIMIT/WITH")
		}
		// SQL Server UNION type precedence differs from MySQL. Validate each
		// output column before allowing implicit set-operation coercions.
		var branches [][]ast.ExprNode
		for _, child := range n.Selects {
			sel, ok := child.(*ast.SelectStmt)
			if !ok || sel.Fields == nil {
				return r.fail("不支持该 UNION 分支")
			}
			fields := []ast.ExprNode{}
			for _, f := range sel.Fields.Fields {
				if f.WildCard != nil {
					return r.fail("UNION 中请使用显式列名，避免无法验证的类型合并")
				}
				fields = append(fields, f.Expr)
			}
			if len(branches) > 0 && len(fields) != len(branches[0]) {
				return r.fail("UNION 分支列数不一致")
			}
			branches = append(branches, fields)
		}
		if len(branches) > 0 {
			for i := range branches[0] {
				args := []ast.ExprNode{}
				for _, b := range branches {
					args = append(args, b[i])
				}
				if !r.compatible(args) {
					return ""
				}
			}
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
			s += " DESC"
		} else {
			s += " ASC"
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
		if k := r.domain(e); k != "number" && k != "null" {
			return r.fail("非数值表达式不能直接用作条件")
		}
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
		if !pred && booleanExpr(n) && !r.compatible([]ast.ExprNode{n.L, n.R}) {
			return ""
		}
		if n.Op == opcode.Plus || n.Op == opcode.Minus || n.Op == opcode.Mul || n.Op == opcode.Div {
			return r.arithmetic(n)
		}
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
		branches := []ast.ExprNode{}
		for _, w := range n.WhenClauses {
			branches = append(branches, w.Result)
		}
		if n.ElseClause != nil {
			branches = append(branches, n.ElseClause)
		}
		if !r.compatible(branches) {
			return ""
		}
		if n.Value != nil {
			for _, w := range n.WhenClauses {
				if !r.compatible([]ast.ExprNode{n.Value, w.Expr}) {
					return ""
				}
			}
		}
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
			if !r.compatible(n.Args[1:]) {
				return ""
			}
			return "CASE WHEN " + r.expr(n.Args[0], true) + " THEN " + r.expr(n.Args[1], false) + " ELSE " + r.expr(n.Args[2], false) + " END"
		}
		if name == "NOW" || name == "CURRENT_TIMESTAMP" {
			if len(n.Args) != 0 {
				return r.fail("暂不支持带精度参数的 NOW/CURRENT_TIMESTAMP")
			}
			return "SYSDATETIME()"
		}
		if name == "IFNULL" {
			if len(n.Args) != 2 {
				return r.fail("IFNULL 参数数量错误")
			}
			name = "COALESCE"
		}
		switch name {
		case "COALESCE", "NULLIF", "ABS", "ROUND", "LOWER", "UPPER", "CONCAT":
		default:
			return r.fail("暂不支持 MySQL 函数 %s", name)
		}
		if name == "COALESCE" || name == "NULLIF" {
			if !r.compatible(n.Args) {
				return ""
			}
		}
		if name == "LOWER" || name == "UPPER" {
			if len(n.Args) != 1 || r.domain(n.Args[0]) != "text" {
				return r.fail("字符函数需要明确的字符参数")
			}
		}
		if name == "ABS" || name == "ROUND" {
			for _, a := range n.Args {
				if _, ok := r.numeric(a); !ok {
					return r.fail("数值函数需要明确的数值参数")
				}
			}
		}
		if name == "CONCAT" {
			for _, a := range n.Args {
				domain := r.domain(a)
				if domain != "text" && domain != "number" && domain != "null" {
					return r.fail("CONCAT 参数类型不明确")
				}
			}
		}
		args := []string{}
		for _, a := range n.Args {
			args = append(args, r.expr(a, false))
		}
		if name == "CONCAT" {
			if len(args) == 0 {
				return r.fail("CONCAT 参数为空")
			}
			checks := []string{}
			parts := []string{}
			for _, a := range args {
				checks = append(checks, a+" IS NULL")
				parts = append(parts, "CAST("+a+" AS nvarchar(max))")
			}
			return "CASE WHEN " + strings.Join(checks, " OR ") + " THEN NULL ELSE " + strings.Join(parts, " + ") + " END"
		}
		if name == "ROUND" && len(args) == 1 {
			args = append(args, "0")
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
		if name == "COUNT" {
			name = "COUNT_BIG"
		}
		avgType := ""
		if name == "SUM" || name == "AVG" {
			t, ok := r.numeric(n.Args[0])
			if !ok {
				return r.fail("无法确定聚合表达式的数值精度")
			}
			if t.p+r.divIncrement > 38 {
				return r.fail("聚合表达式可能超过 38 位精度")
			}
			args = fmt.Sprintf("CAST(%s AS decimal(%d,%d))", args, t.p, t.s)
			if name == "AVG" {
				avgType = fmt.Sprintf("decimal(%d,%d)", t.p+r.divIncrement, t.s+r.divIncrement)
				args = fmt.Sprintf("CAST(%s AS %s)", args, avgType)
			}
		}
		if n.Distinct {
			args = "DISTINCT " + args
		}
		if avgType != "" {
			return "CAST(" + name + "(" + args + ") AS " + avgType + ")"
		}
		return name + "(" + args + ")"
	default:
		return r.fail("不支持的 MySQL 表达式 %T", e)
	}
}

func uintLiteral(e ast.ExprNode) (uint64, bool) {
	v, ok := e.(ast.ValueExpr)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(fmt.Sprint(v.GetValue()), 10, 63)
	return n, err == nil
}

type aliasVisitor struct{ r *renderer }

func (v *aliasVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if t, ok := n.(*ast.TableName); ok {
		v.r.sources[t.Name.O] = true
		v.r.deps[t.Name.O] = true
	}
	if t, ok := n.(*ast.TableSource); ok {
		if base, ok := t.Source.(*ast.TableName); ok && t.AsName.O != "" {
			alias := t.AsName.O
			if old, ok := v.r.aliases[alias]; ok && old != base.Name.O {
				v.r.fail("嵌套查询别名 %s 指向不同对象，需手动适配", alias)
			}
			v.r.aliases[alias] = base.Name.O
		}
	}
	return n, false
}
func (v *aliasVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, true }

type numberType struct{ p, s int }

func (r *renderer) numeric(e ast.ExprNode) (numberType, bool) {
	switch n := e.(type) {
	case *ast.BinaryOperationExpr:
		l, ok := r.numeric(n.L)
		if !ok {
			return numberType{}, false
		}
		rr, ok := r.numeric(n.R)
		if !ok {
			return numberType{}, false
		}
		p, scale := 0, 0
		switch n.Op {
		case opcode.Plus, opcode.Minus:
			scale = max(l.s, rr.s)
			p = max(l.p-l.s, rr.p-rr.s) + scale + 1
		case opcode.Mul:
			p = l.p + rr.p + 1
			scale = l.s + rr.s
		case opcode.Div:
			scale = l.s + r.divIncrement
			p = l.p - l.s + rr.s + scale
		default:
			return numberType{}, false
		}
		return numberType{p, scale}, p <= 38
	case *ast.ParenthesesExpr:
		return r.numeric(n.Expr)
	case *ast.UnaryOperationExpr:
		return r.numeric(n.V)
	case ast.ValueExpr:
		str := fmt.Sprint(n.GetValue())
		if str == "<nil>" {
			return numberType{}, false
		}
		if _, ok := n.GetValue().(string); ok {
			return numberType{}, false
		}
		str = strings.TrimLeft(str, "+-")
		parts := strings.Split(str, ".")
		if len(parts) > 2 {
			return numberType{}, false
		}
		p := len(parts[0])
		s := 0
		if len(parts) == 2 {
			s = len(parts[1])
			p += s
		}
		for _, ch := range str {
			if ch != '.' && (ch < '0' || ch > '9') {
				return numberType{}, false
			}
		}
		return numberType{max(p, 1), s}, p <= 38
	case *ast.ColumnNameExpr:
		table := n.Name.Table.O
		if a, ok := r.aliases[table]; ok {
			table = a
		}
		var found *source.ColumnInfo
		for tn, t := range r.tables {
			if (table != "" && tn != table) || !r.sources[tn] {
				continue
			}
			for _, c := range t.Columns {
				if c.Name == n.Name.Name.O {
					if found != nil {
						return numberType{}, false
					}
					cc := c
					found = &cc
				}
			}
		}
		if found == nil {
			return numberType{}, false
		}
		switch strings.ToLower(found.DataType) {
		case "tinyint":
			return numberType{3, 0}, true
		case "smallint":
			return numberType{5, 0}, true
		case "mediumint":
			return numberType{8, 0}, true
		case "int", "integer":
			return numberType{10, 0}, true
		case "bigint":
			return numberType{20, 0}, true
		case "decimal", "numeric":
			return numberType{int(found.Precision), int(found.Scale)}, found.Precision <= 38
		}
	}
	return numberType{}, false
}
func (r *renderer) arithmetic(n *ast.BinaryOperationExpr) string {
	l, ok := r.numeric(n.L)
	if !ok {
		return r.fail("算术表达式左侧类型/精度不明确，需手动适配")
	}
	rr, ok := r.numeric(n.R)
	if !ok {
		return r.fail("算术表达式右侧类型/精度不明确，需手动适配")
	}
	divisionScale := l.s + r.divIncrement
	divisionPrecision := l.p - l.s + rr.s + divisionScale
	if n.Op == opcode.Div {
		extra := max(0, divisionScale-rr.p-1-l.s)
		l.p += extra
		l.s += extra
		if l.p > 38 || divisionPrecision > 38 {
			return r.fail("除法精度超过 SQL Server 38 位范围")
		}
	}
	p, s := 0, 0
	switch n.Op {
	case opcode.Plus, opcode.Minus:
		s = max(l.s, rr.s)
		p = max(l.p-l.s, rr.p-rr.s) + s + 1
	case opcode.Mul:
		p = l.p + rr.p + 1
		s = l.s + rr.s
	case opcode.Div:
		s = max(6, l.s+rr.p+1)
		p = l.p - l.s + rr.s + s
	}
	if p > 38 {
		return r.fail("算术结果可能超过 SQL Server 38 位精度")
	}
	left := fmt.Sprintf("CAST(%s AS decimal(%d,%d))", r.expr(n.L, false), l.p, l.s)
	right := fmt.Sprintf("CAST(%s AS decimal(%d,%d))", r.expr(n.R, false), rr.p, rr.s)
	if n.Op == opcode.Div {
		right = "NULLIF(" + right + ",0)"
		resultScale := divisionScale
		if resultScale > 38 {
			return r.fail("除法精度超限")
		}
		return fmt.Sprintf("CAST((%s / %s) AS decimal(%d,%d))", left, right, divisionPrecision, resultScale)
	}
	return "(" + left + " " + map[opcode.Op]string{opcode.Plus: "+", opcode.Minus: "-", opcode.Mul: "*"}[n.Op] + " " + right + ")"
}

func (r *renderer) column(e *ast.ColumnNameExpr) *source.ColumnInfo {
	table := e.Name.Table.O
	if a, ok := r.aliases[table]; ok {
		table = a
	}
	var found *source.ColumnInfo
	for tn, t := range r.tables {
		if (table != "" && tn != table) || !r.sources[tn] {
			continue
		}
		for _, c := range t.Columns {
			if c.Name == e.Name.Name.O {
				if found != nil {
					return nil
				}
				cc := c
				found = &cc
			}
		}
	}
	return found
}

// Domain checks reject MySQL's implicit numeric/string coercions rather than
// accidentally inheriting SQL Server's different type precedence rules.
func (r *renderer) domain(e ast.ExprNode) string {
	if _, ok := r.numeric(e); ok {
		return "number"
	}
	switch n := e.(type) {
	case ast.ValueExpr:
		if n.GetValue() == nil {
			return "null"
		}
		if _, ok := n.GetValue().(string); ok {
			return "text"
		}
	case *ast.ColumnNameExpr:
		if c := r.column(n); c != nil {
			switch strings.ToLower(c.DataType) {
			case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
				return "text"
			case "date", "datetime", "timestamp", "time":
				return c.DataType
			}
		}
	case *ast.ParenthesesExpr:
		return r.domain(n.Expr)
	case *ast.FuncCallExpr:
		switch strings.ToUpper(n.FnName.O) {
		case "CONCAT", "LOWER", "UPPER":
			return "text"
		case "ABS", "ROUND":
			return "number"
		case "IF":
			if len(n.Args) == 3 {
				return r.domain(n.Args[1])
			}
		case "IFNULL", "COALESCE", "NULLIF":
			for _, a := range n.Args {
				if k := r.domain(a); k != "null" {
					return k
				}
			}
		}
	case *ast.CaseExpr:
		if len(n.WhenClauses) > 0 {
			return r.domain(n.WhenClauses[0].Result)
		}
	case *ast.AggregateFuncExpr:
		return "number"
	}
	if booleanExpr(e) {
		return "number"
	}
	return "unknown"
}
func (r *renderer) compatible(args []ast.ExprNode) bool {
	maxInteger, maxScale := 0, 0
	for _, a := range args {
		if n, ok := r.numeric(a); ok {
			maxInteger = max(maxInteger, n.p-n.s)
			maxScale = max(maxScale, n.s)
		}
	}
	if maxInteger+maxScale > 38 {
		r.fail("数值类型合并超过 38 位，可能损失小数精度")
		return false
	}
	domain := ""
	for _, a := range args {
		k := r.domain(a)
		if k == "unknown" {
			r.fail("表达式类型不明确，无法保证等价转换")
			return false
		}
		if k == "null" {
			continue
		}
		if domain != "" && domain != k {
			r.fail("混合 %s/%s 类型的隐式转换需手动适配", domain, k)
			return false
		}
		domain = k
	}
	return true
}
