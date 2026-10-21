package mysqltrigger

import (
	"fmt"
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
	sql  string
	kind valueKind
}

func kindOf(t string) valueKind {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexAny(t, "( "); i >= 0 {
		t = t[:i]
	}
	switch t {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric":
		return kindNumber
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
		return kindText
	case "datetime", "timestamp":
		return kindTime
	case "bool", "boolean":
		return kindBool
	}
	return kindUnknown
}
func assignable(target, value valueKind) error {
	if target == kindUnknown || value == kindUnknown {
		return fmt.Errorf("表达式涉及未支持的列类型")
	}
	if value != kindNull && target != value {
		return fmt.Errorf("表达式存在跨类型隐式转换，需要人工适配")
	}
	return nil
}

// MySQL applies the destination column's fractional precision when a trigger
// assigns a temporal value. The table mapping may have a wider PG timestamp,
// so preserve this rounding explicitly for every generated write.
func assignColumn(col *source.ColumnInfo, value expression) (expression, error) {
	kind := kindOf(col.DataType)
	if err := assignable(kind, value.kind); err != nil {
		return expression{}, err
	}
	if kind == kindTime && value.kind != kindNull {
		if col.DateTimePrecision < 0 || col.DateTimePrecision > 6 {
			return expression{}, fmt.Errorf("列 %s 的时间精度无效", col.Name)
		}
		value.sql = fmt.Sprintf("CAST(%s AS timestamp(%d) without time zone)", value.sql, col.DateTimePrecision)
	}
	return value, nil
}
func (c *converter) expression(s string, t *source.TableDDLInfo) (expression, error) {
	// SELECT is only a parser wrapper. Query modifiers must not become a
	// backdoor to accepting text which was never a MySQL scalar expression.
	mode, err := savedMode(c.info.SQLMode)
	if err != nil {
		return expression{}, err
	}
	tokens, err := lex(s, mode.HasANSIQuotesMode(), mode.HasNoBackslashEscapesMode())
	if err != nil {
		return expression{}, err
	}
	if len(tokens) == 0 {
		return expression{}, fmt.Errorf("表达式为空")
	}
	switch tokens[0].word {
	case "ALL", "DISTINCT", "DISTINCTROW", "SQL_CACHE", "SQL_NO_CACHE", "SQL_SMALL_RESULT", "SQL_BIG_RESULT", "SQL_BUFFER_RESULT", "SQL_CALC_FOUND_ROWS", "HIGH_PRIORITY", "LOW_PRIORITY", "STRAIGHT_JOIN":
		return expression{}, fmt.Errorf("表达式中不支持 SELECT 查询修饰符")
	}
	n, err := c.parse("SELECT " + s)
	if err != nil {
		return expression{}, err
	}
	q, ok := n.(*ast.SelectStmt)
	if !ok || q.From != nil || q.Where != nil || q.GroupBy != nil || q.Having != nil || q.OrderBy != nil || q.Limit != nil || q.LockInfo != nil || q.With != nil || q.SelectIntoOpt != nil || len(q.WindowSpecs) > 0 || len(q.TableHints) > 0 || q.Distinct || q.Fields == nil || len(q.Fields.Fields) != 1 || q.Kind != ast.SelectStmtKindSelect || q.AfterSetOperator != nil || len(q.Lists) > 0 {
		return expression{}, fmt.Errorf("仅支持不含查询的单个表达式")
	}
	if opts := q.SelectStmtOpts; opts != nil && (opts.Distinct || opts.ExplicitAll || opts.SQLBigResult || opts.SQLBufferResult || !opts.SQLCache || opts.SQLSmallResult || opts.CalcFoundRows || opts.StraightJoin || opts.Priority != mysql.NoPriority || len(opts.TableHints) > 0) {
		return expression{}, fmt.Errorf("表达式中不支持 SELECT 查询修饰符")
	}
	f := q.Fields.Fields[0]
	if f.Expr == nil || f.WildCard != nil || f.AsName.O != "" {
		return expression{}, fmt.Errorf("表达式不支持星号或别名")
	}
	return c.expr(f.Expr, t)
}
func (c *converter) expr(n ast.ExprNode, t *source.TableDDLInfo) (expression, error) {
	bad := func(msg string) (expression, error) { return expression{}, fmt.Errorf("%s", msg) }
	switch e := n.(type) {
	case ast.ValueExpr:
		switch v := e.GetValue().(type) {
		case nil:
			return expression{"NULL", kindNull}, nil
		case int64:
			if mysql.HasIsBooleanFlag(e.GetType().GetFlag()) {
				if v != 0 {
					return expression{"TRUE", kindBool}, nil
				}
				return expression{"FALSE", kindBool}, nil
			}
			return expression{strconv.FormatInt(v, 10), kindNumber}, nil
		case uint64:
			return expression{strconv.FormatUint(v, 10), kindNumber}, nil
		case string:
			if charset := e.GetType().GetCharset(); charset != "" && charset != "utf8mb4" {
				return bad("字符串字符集需要人工适配")
			}
			if strings.ContainsRune(v, 0) {
				return bad("字符串包含 NUL，需要人工适配")
			}
			return expression{"E'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `'`, `''`) + "'", kindText}, nil
		case *test_driver.MyDecimal:
			return expression{v.String(), kindNumber}, nil
		default:
			return bad("不支持浮点、二进制或特殊字面量")
		}
	case *ast.ColumnNameExpr:
		cn := e.Name
		var col *source.ColumnInfo
		var err error
		prefix := ""
		if strings.EqualFold(cn.Table.O, "NEW") || strings.EqualFold(cn.Table.O, "OLD") {
			if cn.Schema.O != "" {
				return bad("NEW/OLD 不允许 schema 限定")
			}
			prefix = strings.ToUpper(cn.Table.O)
			if prefix == "NEW" && c.info.Event == "DELETE" || prefix == "OLD" && c.info.Event == "INSERT" {
				return bad("触发事件不允许此 NEW/OLD 引用")
			}
			col, err = c.column(c.tables[c.info.TableName], cn.Name.O)
			if err == nil && c.info.Timing == "BEFORE" && c.info.Event == "INSERT" && strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				return bad("BEFORE INSERT 读取自增列的 NEW 值需要人工适配")
			}
			if err == nil && prefix == "NEW" && c.info.Event == "UPDATE" && strings.Contains(strings.ToLower(col.Extra), "on update") {
				return bad("读取依赖 ON UPDATE 自动更新的 NEW 列需要人工适配")
			}
		} else {
			if t == nil {
				return bad("表达式仅允许 NEW/OLD 列引用")
			}
			if cn.Schema.O != "" && cn.Schema.O != c.info.Schema {
				return bad("跨库列引用需要人工适配")
			}
			if cn.Table.O != "" && cn.Table.O != t.TableName {
				return bad("列引用表不匹配或使用了别名")
			}
			col, err = c.column(t, cn.Name.O)
			prefix = quote(c.opt.Name(t.TableName))
		}
		if err != nil {
			return expression{}, err
		}
		kind := kindOf(col.DataType)
		if kind == kindUnknown {
			return bad("列类型 " + col.DataType + " 需要人工适配")
		}
		// A PG column may have a wider typmod than its MySQL source. Read
		// temporal records at the source precision before any COALESCE/write.
		return assignColumn(col, expression{prefix + "." + quote(c.opt.Name(col.Name)), kind})
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
		return expression{"(" + v.sql + op + ")", kindBool}, nil
	case *ast.UnaryOperationExpr:
		v, err := c.expr(e.V, t)
		if err != nil {
			return expression{}, err
		}
		switch e.Op {
		case opcode.Not, opcode.Not2:
			if v.kind != kindBool {
				return bad("NOT 仅支持布尔表达式")
			}
			return expression{"(NOT " + v.sql + ")", kindBool}, nil
		case opcode.Plus, opcode.Minus:
			if v.kind != kindNumber {
				return bad("一元正负号仅支持数值")
			}
			op := "+"
			if e.Op == opcode.Minus {
				op = "-"
			}
			return expression{"(" + op + v.sql + ")", kindNumber}, nil
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
		op := ""
		kind := kindBool
		switch e.Op {
		case opcode.LogicAnd, opcode.LogicOr:
			if a.kind != kindBool || b.kind != kindBool {
				return bad("AND/OR 仅支持布尔表达式")
			}
			op = "AND"
			if e.Op == opcode.LogicOr {
				op = "OR"
			}
		case opcode.EQ, opcode.NE, opcode.LT, opcode.LE, opcode.GT, opcode.GE, opcode.NullEQ:
			if a.kind == kindNull || b.kind == kindNull {
				return bad("NULL 比较请使用 IS NULL/IS NOT NULL")
			}
			if a.kind != b.kind || a.kind == kindUnknown {
				return bad("比较存在跨类型隐式转换")
			}
			// MySQL collations often compare case/accent-insensitively. Do not
			// claim equivalent semantics for textual predicates on a PG target.
			if a.kind == kindText {
				return bad("字符比较依赖源库排序规则，需要人工适配")
			}
			if a.kind == kindTime {
				return bad("时间比较依赖源列精度和会话时区，需要人工适配")
			}
			op = map[opcode.Op]string{opcode.EQ: "=", opcode.NE: "<>", opcode.LT: "<", opcode.LE: "<=", opcode.GT: ">", opcode.GE: ">=", opcode.NullEQ: "IS NOT DISTINCT FROM"}[e.Op]
		case opcode.Plus, opcode.Minus, opcode.Mul:
			if a.kind != kindNumber || b.kind != kindNumber {
				return bad("算术仅支持明确的数值操作数")
			}
			kind = kindNumber
			op = map[opcode.Op]string{opcode.Plus: "+", opcode.Minus: "-", opcode.Mul: "*"}[e.Op]
		default:
			return bad("运算符 " + e.Op.String() + " 需要人工适配")
		}
		return expression{"(" + a.sql + " " + op + " " + b.sql + ")", kind}, nil
	case *ast.FuncCallExpr:
		if e.Schema.O != "" {
			return bad("不支持用户定义或跨 schema 函数")
		}
		fn := strings.ToLower(e.FnName.O)
		if fn == "now" || fn == "current_timestamp" {
			fsp := int64(0)
			if len(e.Args) > 1 {
				return bad("时间函数参数无效")
			}
			if len(e.Args) == 1 {
				literal, ok := e.Args[0].(ast.ValueExpr)
				if !ok {
					return bad("时间精度必须是 0 或 6")
				}
				v, ok := literal.GetValue().(int64)
				if !ok {
					return bad("时间精度必须是 0 或 6")
				}
				fsp = v
			}
			if fsp == 0 {
				return expression{"pg_catalog.date_trunc('second', pg_catalog.statement_timestamp())::timestamp without time zone", kindTime}, nil
			}
			if fsp == 6 {
				return expression{"pg_catalog.statement_timestamp()::timestamp without time zone", kindTime}, nil
			}
			return bad("时间精度仅支持 0 或 6")
		}
		if fn != "ifnull" && fn != "coalesce" {
			return bad("函数 " + e.FnName.O + " 需要人工适配")
		}
		if len(e.Args) < 2 || fn == "ifnull" && len(e.Args) != 2 {
			return bad("NULL 函数参数数量无效")
		}
		var parts []string
		kind := kindNull
		for _, arg := range e.Args {
			v, err := c.expr(arg, t)
			if err != nil {
				return expression{}, err
			}
			if v.kind != kindNull {
				if kind == kindNull {
					kind = v.kind
				} else if kind != v.kind {
					return bad("NULL 函数混合类型需要人工适配")
				}
			}
			parts = append(parts, v.sql)
		}
		return expression{"COALESCE(" + strings.Join(parts, ", ") + ")", kind}, nil
	}
	return bad(fmt.Sprintf("表达式 %T 需要人工适配", n))
}
