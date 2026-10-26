package mysqldmtrigger

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	"github.com/pingcap/tidb/pkg/parser/test_driver"
)

type valueKind int

const (
	kindUnknown valueKind = iota
	kindNull
	kindNumber
	kindText
	kindTime
	kindBool
)

type expression struct {
	sql   string
	kind  valueKind
	col   *source.ColumnInfo
	table string
}

func kindOf(s string) valueKind {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "( "); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "number", "float", "double", "real", "year", "bit":
		return kindNumber
	case "char", "varchar", "varchar2", "nvarchar2", "tinytext", "text", "mediumtext", "longtext":
		return kindText
	case "date", "datetime", "timestamp", "time":
		return kindTime
	case "bool", "boolean":
		return kindBool
	}
	return kindUnknown
}
func assignable(a, b valueKind) error {
	if a == kindUnknown || b == kindUnknown {
		return fmt.Errorf("表达式涉及未支持的类型")
	}
	if b != kindNull && a != b {
		return fmt.Errorf("跨类型隐式转换需要人工适配")
	}
	return nil
}
func (c *converter) predicate(v expression) (string, error) {
	switch v.kind {
	case kindBool:
		return v.sql, nil
	case kindNumber:
		return "(" + v.sql + " <> 0)", nil
	case kindNull:
		return "(NULL = 1)", nil
	}
	return "", fmt.Errorf("条件的字符或日期隐式布尔转换需要人工适配")
}
func (c *converter) expression(s string, t *source.TableDDLInfo) (expression, error) {
	n, err := c.parse("SELECT " + s)
	if err != nil {
		return expression{}, err
	}
	q, ok := n.(*ast.SelectStmt)
	if !ok || q.From != nil || q.Where != nil || q.GroupBy != nil || q.Having != nil || q.OrderBy != nil || q.Limit != nil || q.LockInfo != nil || q.With != nil || q.SelectIntoOpt != nil || len(q.WindowSpecs) > 0 || len(q.TableHints) > 0 || q.Distinct || q.Fields == nil || len(q.Fields.Fields) != 1 || q.Kind != ast.SelectStmtKindSelect || q.AfterSetOperator != nil || len(q.Lists) > 0 {
		return expression{}, fmt.Errorf("需要不含查询的单个标量表达式")
	}
	if opts := q.SelectStmtOpts; opts != nil && (opts.Distinct || opts.ExplicitAll || opts.SQLBigResult || opts.SQLBufferResult || !opts.SQLCache || opts.SQLSmallResult || opts.CalcFoundRows || opts.StraightJoin || opts.Priority != mysql.NoPriority || len(opts.TableHints) > 0) {
		return expression{}, fmt.Errorf("表达式不支持 SELECT 修饰符")
	}
	f := q.Fields.Fields[0]
	if f.Expr == nil || f.WildCard != nil || f.AsName.O != "" {
		return expression{}, fmt.Errorf("表达式不支持星号或别名")
	}
	return c.expr(f.Expr, t)
}
func (c *converter) expr(n ast.ExprNode, t *source.TableDDLInfo) (expression, error) {
	bad := func(s string) (expression, error) { return expression{}, fmt.Errorf("%s", s) }
	switch e := n.(type) {
	case ast.ValueExpr:
		switch v := e.GetValue().(type) {
		case nil:
			return expression{sql: "NULL", kind: kindNull}, nil
		case int64:
			if mysql.HasIsBooleanFlag(e.GetType().GetFlag()) {
				if v != 0 {
					return expression{sql: "1", kind: kindNumber}, nil
				}
				return expression{sql: "0", kind: kindNumber}, nil
			}
			return expression{sql: strconv.FormatInt(v, 10), kind: kindNumber}, nil
		case uint64:
			return expression{sql: strconv.FormatUint(v, 10), kind: kindNumber}, nil
		case string:
			if cs := e.GetType().GetCharset(); cs != "" && cs != "utf8mb4" && cs != "utf8" {
				return bad("字符串字符集需要人工适配")
			}
			if strings.ContainsRune(v, 0) {
				return bad("字符串包含 NUL")
			}
			if v == "" && c.opt.Capabilities.EmptyStringIsNull {
				return bad("目标将空字符串视为 NULL，无法保留此字符串表达式")
			}
			return expression{sql: "'" + strings.ReplaceAll(v, "'", "''") + "'", kind: kindText}, nil
		case *test_driver.MyDecimal:
			return expression{sql: v.String(), kind: kindNumber}, nil
		default:
			return bad("浮点或二进制字面量需要人工适配")
		}
	case *ast.ColumnNameExpr:
		cn := e.Name
		if cn.Schema.O == "" && cn.Table.O == "" {
			if v, ok := c.lookup(cn.Name.O); ok {
				return expression{sql: v.name, kind: v.kind}, nil
			}
		}
		var col *source.ColumnInfo
		var err error
		var owner *source.TableDDLInfo
		var prefix string
		if strings.EqualFold(cn.Table.O, "NEW") || strings.EqualFold(cn.Table.O, "OLD") {
			if cn.Schema.O != "" {
				return bad("NEW/OLD 不允许 schema 限定")
			}
			row := strings.ToUpper(cn.Table.O)
			if row == "NEW" && strings.EqualFold(c.info.Event, "DELETE") || row == "OLD" && strings.EqualFold(c.info.Event, "INSERT") {
				return bad("当前触发事件不允许此 NEW/OLD 引用")
			}
			owner = c.tables[c.info.TableName]
			col, err = c.column(owner, cn.Name.O)
			prefix = ":" + row
			if err == nil && row == "NEW" && strings.EqualFold(c.info.Timing, "BEFORE") && strings.EqualFold(c.info.Event, "INSERT") && strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				return bad("BEFORE INSERT 的 NEW 自增值需要人工适配")
			}
			if err == nil && row == "NEW" && strings.EqualFold(c.info.Event, "UPDATE") && strings.Contains(strings.ToLower(col.Extra), "on update") {
				return bad("依赖 ON UPDATE 自动列值需要人工适配")
			}
		} else {
			if t == nil {
				return bad("未声明的局部变量或缺少表上下文的列 " + cn.Name.O)
			}
			if cn.Schema.O != "" && cn.Schema.O != c.info.Schema {
				return bad("跨源数据库列引用需要人工适配")
			}
			if cn.Table.O != "" && !strings.EqualFold(cn.Table.O, t.TableName) {
				return bad("列引用表不匹配或使用了未支持的别名")
			}
			owner = t
			col, err = c.column(t, cn.Name.O)
			prefix = c.target(t)
			if c.allowAggregate && c.aggregateDepth == 0 {
				c.hasPlainQueryColumn = true
			}
		}
		if err != nil {
			return expression{}, err
		}
		if col.Generated != "" || strings.Contains(strings.ToLower(col.Extra), "generated") {
			return bad("生成列引用需要人工适配")
		}
		k := kindOf(col.DataType)
		if k == kindUnknown {
			return bad("列类型 " + col.DataType + " 需要人工适配")
		}
		return expression{sql: prefix + "." + quote(c.columnName(owner.TableName, col.Name)), kind: k, col: col, table: owner.TableName}, nil
	case *ast.ParenthesesExpr:
		v, err := c.expr(e.Expr, t)
		v.sql = "(" + v.sql + ")"
		return v, err
	case *ast.IsNullExpr:
		v, err := c.expr(e.Expr, t)
		if err != nil {
			return expression{}, err
		}
		op := " IS NULL"
		if e.Not {
			op = " IS NOT NULL"
		}
		return expression{sql: "(" + v.sql + op + ")", kind: kindBool}, nil
	case *ast.IsTruthExpr:
		v, err := c.expr(e.Expr, t)
		if err != nil {
			return expression{}, err
		}
		p, err := c.predicate(v)
		if err != nil {
			return expression{}, err
		}
		test := "1"
		if e.True == 0 {
			test = "0"
		}
		op := "="
		if e.Not {
			op = "<>"
		}
		return expression{sql: "((CASE WHEN " + p + " THEN 1 WHEN NOT " + p + " THEN 0 ELSE -1 END) " + op + " " + test + ")", kind: kindBool}, nil
	case *ast.UnaryOperationExpr:
		v, err := c.expr(e.V, t)
		if err != nil {
			return expression{}, err
		}
		switch e.Op {
		case opcode.Not, opcode.Not2:
			p, err := c.predicate(v)
			return expression{sql: "(NOT " + p + ")", kind: kindBool}, err
		case opcode.Plus, opcode.Minus:
			if v.kind != kindNumber && v.kind != kindNull {
				return bad("一元正负号仅支持数值")
			}
			op := "+"
			if e.Op == opcode.Minus {
				op = "-"
			}
			return expression{sql: "(" + op + v.sql + ")", kind: kindNumber}, nil
		}
		return bad("不支持此一元运算")
	case *ast.BinaryOperationExpr:
		a, err := c.expr(e.L, t)
		if err != nil {
			return expression{}, err
		}
		b, err := c.expr(e.R, t)
		if err != nil {
			return expression{}, err
		}
		switch e.Op {
		case opcode.LogicAnd, opcode.LogicOr, opcode.LogicXor:
			x, err := c.predicate(a)
			if err != nil {
				return expression{}, err
			}
			y, err := c.predicate(b)
			if err != nil {
				return expression{}, err
			}
			op := "AND"
			if e.Op == opcode.LogicOr {
				op = "OR"
			}
			if e.Op == opcode.LogicXor {
				return expression{sql: "((" + x + " AND NOT " + y + ") OR (NOT " + x + " AND " + y + "))", kind: kindBool}, nil
			}
			return expression{sql: "(" + x + " " + op + " " + y + ")", kind: kindBool}, nil
		case opcode.EQ, opcode.NE, opcode.LT, opcode.LE, opcode.GT, opcode.GE, opcode.NullEQ:
			return c.compare(e.Op, a, b)
		case opcode.Plus, opcode.Minus, opcode.Mul, opcode.Div, opcode.IntDiv, opcode.Mod:
			if a.kind != kindNumber && a.kind != kindNull || b.kind != kindNumber && b.kind != kindNull {
				return bad("算术涉及跨类型转换")
			}
			if e.Op == opcode.Div || e.Op == opcode.IntDiv || e.Op == opcode.Mod {
				if err := c.checkDivisor(e.R); err != nil {
					return expression{}, err
				}
			}
			if e.Op == opcode.Mod {
				return expression{sql: "MOD(" + a.sql + ", NULLIF(" + b.sql + ", 0))", kind: kindNumber}, nil
			}
			op := map[opcode.Op]string{opcode.Plus: "+", opcode.Minus: "-", opcode.Mul: "*", opcode.Div: "/", opcode.IntDiv: "/"}[e.Op]
			rhs := b.sql
			if e.Op == opcode.Div || e.Op == opcode.IntDiv {
				rhs = "NULLIF(" + rhs + ", 0)"
			}
			s := "(" + a.sql + " " + op + " " + rhs + ")"
			if e.Op == opcode.IntDiv {
				s = "TRUNC(" + s + ")"
			}
			return expression{sql: s, kind: kindNumber}, nil
		}
		return bad("运算符 " + e.Op.String() + " 需要人工适配")
	case *ast.FuncCallExpr:
		return c.function(e, t)
	case *ast.AggregateFuncExpr:
		if !c.allowAggregate || c.aggregateDepth != 0 || e.Distinct || e.Order != nil || len(e.Args) != 1 {
			return bad("聚合只支持 SELECT 字段中的单层 COUNT/SUM/AVG/MIN/MAX，不支持 DISTINCT 或聚合排序")
		}
		fn := strings.ToUpper(e.F)
		if fn != "COUNT" && fn != "SUM" && fn != "AVG" && fn != "MIN" && fn != "MAX" {
			return bad("聚合函数 " + fn + " 需要人工适配")
		}
		c.aggregateDepth++
		v, err := c.expr(e.Args[0], t)
		c.aggregateDepth--
		if err != nil {
			return expression{}, err
		}
		if fn != "COUNT" && v.kind != kindNumber && v.kind != kindNull && !((fn == "MIN" || fn == "MAX") && v.kind == kindTime) {
			return bad("聚合函数 " + fn + " 的字符排序或参数类型需要人工适配")
		}
		c.hasAggregate = true
		k := v.kind
		if fn == "COUNT" || fn == "SUM" || fn == "AVG" {
			k = kindNumber
		}
		return expression{sql: fn + "(" + v.sql + ")", kind: k}, nil
	case *ast.CaseExpr:
		var base expression
		var err error
		if e.Value != nil {
			base, err = c.expr(e.Value, t)
			if err != nil {
				return expression{}, err
			}
		}
		var out strings.Builder
		out.WriteString("CASE")
		k := kindNull
		for _, w := range e.WhenClauses {
			cond, err := c.expr(w.Expr, t)
			if err != nil {
				return expression{}, err
			}
			var p string
			if e.Value != nil {
				v, err := c.compare(opcode.EQ, base, cond)
				if err != nil {
					return expression{}, err
				}
				p = v.sql
			} else {
				p, err = c.predicate(cond)
				if err != nil {
					return expression{}, err
				}
			}
			v, err := c.expr(w.Result, t)
			if err != nil {
				return expression{}, err
			}
			k, err = mergeKind(k, v.kind)
			if err != nil {
				return expression{}, err
			}
			out.WriteString(" WHEN " + p + " THEN " + v.sql)
		}
		if e.ElseClause != nil {
			v, err := c.expr(e.ElseClause, t)
			if err != nil {
				return expression{}, err
			}
			k, err = mergeKind(k, v.kind)
			if err != nil {
				return expression{}, err
			}
			out.WriteString(" ELSE " + v.sql)
		}
		out.WriteString(" END")
		return expression{sql: "(" + out.String() + ")", kind: k}, nil
	case *ast.PatternInExpr:
		if e.Sel != nil || len(e.List) == 0 {
			return bad("IN 子查询需要人工适配")
		}
		v, err := c.expr(e.Expr, t)
		if err != nil {
			return expression{}, err
		}
		var parts []string
		for _, n := range e.List {
			a, err := c.expr(n, t)
			if err != nil {
				return expression{}, err
			}
			cmp, err := c.compare(opcode.EQ, v, a)
			if err != nil {
				return expression{}, err
			}
			parts = append(parts, cmp.sql)
		}
		s := "(" + strings.Join(parts, " OR ") + ")"
		if e.Not {
			s = "(NOT " + s + ")"
		}
		return expression{sql: s, kind: kindBool}, nil
	case *ast.BetweenExpr:
		v, err := c.expr(e.Expr, t)
		if err != nil {
			return expression{}, err
		}
		lo, err := c.expr(e.Left, t)
		if err != nil {
			return expression{}, err
		}
		hi, err := c.expr(e.Right, t)
		if err != nil {
			return expression{}, err
		}
		a, err := c.compare(opcode.GE, v, lo)
		if err != nil {
			return expression{}, err
		}
		b, err := c.compare(opcode.LE, v, hi)
		if err != nil {
			return expression{}, err
		}
		s := "(" + a.sql + " AND " + b.sql + ")"
		if e.Not {
			s = "(NOT " + s + ")"
		}
		return expression{sql: s, kind: kindBool}, nil
	}
	return bad(fmt.Sprintf("表达式 %T 需要人工适配", n))
}
func mergeKind(a, b valueKind) (valueKind, error) {
	if a == kindNull {
		return b, nil
	}
	if b == kindNull || a == b {
		return a, nil
	}
	return kindUnknown, fmt.Errorf("表达式混合类型需要人工适配")
}
func (c *converter) compare(op opcode.Op, a, b expression) (expression, error) {
	if a.kind != kindNull && b.kind != kindNull && a.kind != b.kind {
		return expression{}, fmt.Errorf("比较涉及跨类型隐式转换")
	}
	if a.kind == kindText || b.kind == kindText {
		if c.opt.Capabilities.EmptyStringIsNull {
			return expression{}, fmt.Errorf("目标空字符串与 NULL 无法区分，字符比较需要人工适配")
		}
		if op != opcode.EQ && op != opcode.NE && op != opcode.NullEQ {
			return expression{}, fmt.Errorf("字符大小比较依赖目标字符编码，需要人工适配")
		}
		var err error
		a, b, err = c.textKeys(a, b)
		if err != nil {
			return expression{}, err
		}
	}
	cmp := map[opcode.Op]string{opcode.EQ: "=", opcode.NE: "<>", opcode.LT: "<", opcode.LE: "<=", opcode.GT: ">", opcode.GE: ">="}[op]
	if op == opcode.NullEQ {
		return expression{sql: "(CASE WHEN " + a.sql + " IS NULL AND " + b.sql + " IS NULL THEN 1 WHEN " + a.sql + " IS NULL OR " + b.sql + " IS NULL THEN 0 WHEN " + a.sql + " = " + b.sql + " THEN 1 ELSE 0 END = 1)", kind: kindBool}, nil
	}
	if cmp == "" {
		return expression{}, fmt.Errorf("不支持比较运算")
	}
	return expression{sql: "(" + a.sql + " " + cmp + " " + b.sql + ")", kind: kindBool}, nil
}

// Text equality uses binary keys so the target database collation never chooses
// the result. Unknown source collation, including implicit local collation, is rejected.
func (c *converter) textKeys(a, b expression) (expression, expression, error) {
	chosen := ""
	for _, v := range []expression{a, b} {
		if v.kind == kindNull {
			continue
		}
		if v.col == nil {
			if v.table != "" {
				return a, b, fmt.Errorf("字符表达式的排序规则无法确定")
			}
			continue
		}
		var collation string
		if c.opt.CharacterMetadata != nil {
			for _, m := range c.opt.CharacterMetadata.Columns {
				if m.Schema == c.info.Schema && m.Table == v.table && m.Name == v.col.Name {
					if collation != "" {
						return a, b, fmt.Errorf("源字符排序规则重复")
					}
					if m.Charset != "utf8mb4" && m.Charset != "utf8" {
						return a, b, fmt.Errorf("源字符集需要人工适配")
					}
					collation = m.Collation
				}
			}
		}
		if collation != "utf8mb4_bin" && collation != "utf8_bin" && collation != "utf8mb4_0900_bin" {
			return a, b, fmt.Errorf("字符比较的源排序规则 %s 尚不支持严格转换", collation)
		}
		if chosen != "" && chosen != collation {
			return a, b, fmt.Errorf("混合源字符排序规则需要人工适配")
		}
		chosen = collation
	}
	if chosen == "" {
		return a, b, fmt.Errorf("字符比较缺少可确定的源列排序规则")
	}
	key := func(v expression) expression {
		if v.kind == kindNull {
			return v
		}
		s := v.sql
		if chosen != "utf8mb4_0900_bin" {
			s = "RTRIM(" + s + ", ' ')"
		}
		v.sql = "RAWTOHEX(UTL_RAW.CAST_TO_RAW(" + s + "))"
		return v
	}
	return key(a), key(b), nil
}
func (c *converter) function(e *ast.FuncCallExpr, t *source.TableDDLInfo) (expression, error) {
	if e.Schema.O != "" {
		return expression{}, fmt.Errorf("用户定义或跨 schema 函数需要人工适配")
	}
	fn := strings.ToLower(e.FnName.O)
	bad := func() (expression, error) {
		return expression{}, fmt.Errorf("函数 %s 的参数或语义需要人工适配", e.FnName.O)
	}
	if fn == "now" || fn == "current_timestamp" || fn == "localtimestamp" {
		fsp := int64(0)
		if len(e.Args) > 1 {
			return bad()
		}
		if len(e.Args) == 1 {
			v, ok := e.Args[0].(ast.ValueExpr)
			if !ok {
				return bad()
			}
			fsp, ok = v.GetValue().(int64)
			if !ok || fsp < 0 || fsp > 6 {
				return bad()
			}
		}
		return expression{sql: fmt.Sprintf("CAST(LOCALTIMESTAMP(%d) AS TIMESTAMP(%d))", fsp, fsp), kind: kindTime}, nil
	}
	if fn == "curdate" || fn == "current_date" {
		if len(e.Args) != 0 {
			return bad()
		}
		return expression{sql: "CURRENT_DATE()", kind: kindTime}, nil
	}
	if fn == "curtime" || fn == "current_time" {
		fsp := int64(0)
		if len(e.Args) > 1 {
			return bad()
		}
		if len(e.Args) == 1 {
			v, ok := e.Args[0].(ast.ValueExpr)
			if !ok {
				return bad()
			}
			fsp, ok = v.GetValue().(int64)
			if !ok || fsp < 0 || fsp > 6 {
				return bad()
			}
		}
		return expression{sql: fmt.Sprintf("CAST(CURRENT_TIME(%d) AS TIME(%d))", fsp, fsp), kind: kindTime}, nil
	}
	var args []expression
	for _, arg := range e.Args {
		v, err := c.expr(arg, t)
		if err != nil {
			return expression{}, err
		}
		args = append(args, v)
	}
	sqls := make([]string, len(args))
	for i, v := range args {
		sqls[i] = v.sql
	}
	if fn == "concat" {
		if len(args) == 0 {
			return bad()
		}
		if c.opt.Capabilities.EmptyStringIsNull {
			return expression{}, fmt.Errorf("目标将空字符串视为 NULL，无法保留 CONCAT 语义")
		}
		var nulls, parts []string
		for _, v := range args {
			if v.kind == kindNull {
				return expression{sql: "NULL", kind: kindNull}, nil
			}
			s := v.sql
			switch v.kind {
			case kindText:
			case kindNumber:
				var err error
				s, err = c.numericText(v)
				if err != nil {
					return expression{}, err
				}
			default:
				return bad()
			}
			nulls = append(nulls, v.sql+" IS NULL")
			parts = append(parts, s)
		}
		return expression{sql: "(CASE WHEN " + strings.Join(nulls, " OR ") + " THEN NULL ELSE " + strings.Join(parts, " || ") + " END)", kind: kindText, table: "derived"}, nil
	}
	if fn == "ifnull" || fn == "coalesce" {
		if len(args) < 2 || fn == "ifnull" && len(args) != 2 {
			return bad()
		}
		k := kindNull
		for _, v := range args {
			var err error
			k, err = mergeKind(k, v.kind)
			if err != nil {
				return expression{}, err
			}
		}
		return expression{sql: "COALESCE(" + strings.Join(sqls, ", ") + ")", kind: k, table: derivedTable(k)}, nil
	}
	if fn == "nullif" {
		if len(args) != 2 {
			return bad()
		}
		cmp, err := c.compare(opcode.EQ, args[0], args[1])
		if err != nil {
			return expression{}, err
		}
		return expression{sql: "(CASE WHEN " + cmp.sql + " THEN NULL ELSE " + args[0].sql + " END)", kind: args[0].kind, table: derivedTable(args[0].kind)}, nil
	}
	if fn == "if" {
		if len(args) != 3 {
			return bad()
		}
		p, err := c.predicate(args[0])
		if err != nil {
			return expression{}, err
		}
		k, err := mergeKind(args[1].kind, args[2].kind)
		if err != nil {
			return expression{}, err
		}
		return expression{sql: "(CASE WHEN " + p + " THEN " + args[1].sql + " ELSE " + args[2].sql + " END)", kind: k, table: derivedTable(k)}, nil
	}
	switch fn {
	case "abs", "ceil", "ceiling", "floor", "sign":
		if len(args) != 1 || args[0].kind != kindNumber && args[0].kind != kindNull {
			return bad()
		}
		name := strings.ToUpper(fn)
		if fn == "ceiling" {
			name = "CEIL"
		}
		return expression{sql: name + "(" + sqls[0] + ")", kind: kindNumber}, nil
	case "round", "truncate":
		if len(args) < 1 || len(args) > 2 {
			return bad()
		}
		for _, v := range args {
			if v.kind != kindNumber && v.kind != kindNull {
				return bad()
			}
		}
		name := "ROUND"
		if fn == "truncate" {
			name = "TRUNC"
		}
		return expression{sql: name + "(" + strings.Join(sqls, ", ") + ")", kind: kindNumber}, nil
	case "mod":
		if len(args) != 2 || args[0].kind != kindNumber || args[1].kind != kindNumber {
			return bad()
		}
		if err := c.checkDivisor(e.Args[1]); err != nil {
			return expression{}, err
		}
		return expression{sql: "MOD(" + sqls[0] + ", NULLIF(" + sqls[1] + ", 0))", kind: kindNumber}, nil
	case "upper", "ucase", "lower", "lcase", "ltrim", "rtrim":
		if len(args) != 1 || args[0].kind != kindText && args[0].kind != kindNull {
			return bad()
		}
		name := strings.ToUpper(fn)
		if fn == "ucase" {
			name = "UPPER"
		}
		if fn == "lcase" {
			name = "LOWER"
		}
		return expression{sql: name + "(" + sqls[0] + ")", kind: kindText, table: "derived"}, nil
	case "length", "octet_length", "char_length", "character_length":
		if len(args) != 1 || args[0].kind != kindText && args[0].kind != kindNull {
			return bad()
		}
		name := "LENGTH"
		if fn == "length" || fn == "octet_length" {
			return expression{}, fmt.Errorf("字节长度依赖源与目标字符集，需人工适配；字符长度可使用 CHAR_LENGTH")
		}
		return expression{sql: name + "(" + sqls[0] + ")", kind: kindNumber}, nil
	case "replace":
		if len(args) != 3 {
			return bad()
		}
		for _, v := range args {
			if v.kind != kindText && v.kind != kindNull {
				return bad()
			}
		}
		return expression{sql: "REPLACE(" + strings.Join(sqls, ", ") + ")", kind: kindText, table: "derived"}, nil
	case "substring", "substr":
		if len(args) < 2 || len(args) > 3 || args[0].kind != kindText && args[0].kind != kindNull {
			return bad()
		}
		for _, v := range args[1:] {
			if v.kind != kindNumber {
				return bad()
			}
		}
		s := "SUBSTR(" + strings.Join(sqls, ", ") + ")"
		condition := sqls[1] + " = 0"
		if len(args) == 3 {
			condition += " OR " + sqls[2] + " < 1"
		}
		s = "(CASE WHEN " + condition + " THEN '' ELSE " + s + " END)"
		if c.opt.Capabilities.EmptyStringIsNull {
			return bad()
		}
		return expression{sql: s, kind: kindText, table: "derived"}, nil
	}
	return expression{}, fmt.Errorf("函数 %s 尚不支持自动转换", e.FnName.O)
}
func derivedTable(k valueKind) string {
	if k == kindText {
		return "derived"
	}
	return ""
}
func (c *converter) numericText(v expression) (string, error) {
	if v.col == nil {
		literal := strings.Trim(v.sql, "()")
		if numericLiteral.MatchString(literal) {
			return "'" + literal + "'", nil
		}
		return "", fmt.Errorf("CONCAT 的复合数值或局部变量格式需要人工适配")
	}
	base := strings.ToLower(strings.TrimSpace(v.col.DataType))
	if strings.Contains(base, "float") || base == "double" || base == "real" {
		return "", fmt.Errorf("CONCAT 浮点格式需要人工适配")
	}
	var columnType string
	if c.opt.CharacterMetadata != nil {
		for _, m := range c.opt.CharacterMetadata.NumericColumns {
			if m.Schema == c.info.Schema && m.Table == v.table && m.Name == v.col.Name {
				if columnType != "" {
					return "", fmt.Errorf("源数值格式快照重复")
				}
				columnType = m.ColumnType
			}
		}
	}
	if columnType == "" {
		return "", fmt.Errorf("CONCAT 数值列缺少源 COLUMN_TYPE 格式快照")
	}
	if strings.Contains(strings.ToLower(columnType), "zerofill") {
		return "", fmt.Errorf("CONCAT ZEROFILL 格式需要人工适配")
	}
	if base == "decimal" || base == "numeric" {
		p, s := v.col.Precision, v.col.Scale
		if p <= 0 || p > 38 || s < 0 || s > p {
			return "", fmt.Errorf("CONCAT 定点数格式超出支持范围")
		}
		format := "FM" + strings.Repeat("9", int(p-s)) + "0"
		if s > 0 {
			format += "." + strings.Repeat("0", int(s))
		}
		return "TO_CHAR(" + v.sql + ", '" + format + "', 'NLS_NUMERIC_CHARACTERS=''.,''')", nil
	}
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "year":
		return "TO_CHAR(" + v.sql + ", 'FM999999999999999999990')", nil
	}
	return "", fmt.Errorf("CONCAT 数值格式尚不支持")
}

var numericLiteral = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)

// In strict trigger SQL modes, turning division by zero into NULL would change
// a source write error into a successful target write. Only accept divisors
// whose nonzero value is established syntactically, without folding mutable
// columns, local variables, or function calls.
func (c *converter) checkDivisor(n ast.ExprNode) error {
	if c.mode.HasStrictMode() && c.mode.HasErrorForDivisionByZeroMode() && !nonzeroLiteral(n) {
		return fmt.Errorf("严格 SQL_MODE 的除零异常语义需要人工适配；除数必须是可确认非零的数值字面量")
	}
	return nil
}

func nonzeroLiteral(n ast.ExprNode) bool {
	switch e := n.(type) {
	case *ast.ParenthesesExpr:
		return nonzeroLiteral(e.Expr)
	case *ast.UnaryOperationExpr:
		return (e.Op == opcode.Plus || e.Op == opcode.Minus) && nonzeroLiteral(e.V)
	case ast.ValueExpr:
		switch v := e.GetValue().(type) {
		case int64:
			return v != 0
		case uint64:
			return v != 0
		case *test_driver.MyDecimal:
			r, ok := new(big.Rat).SetString(v.String())
			return ok && r.Sign() != 0
		}
	}
	return false
}
