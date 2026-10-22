package mysqltrigger

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

// ErrNativeUserVariablesRequired lets callers attach a failed target capability
// probe only to conversions which actually need native session-variable writes.
var ErrNativeUserVariablesRequired = errors.New("会话变量赋值需要 Kingbase MySQL 模式的原生用户变量能力")

var nativeUserVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,63}$`)
var nativeSignedIntegerType = regexp.MustCompile(`(?i)^(tinyint|smallint|mediumint|int|integer|bigint)(?:\([0-9]+\))?$`)

func (c *converter) setNativeUserVariable(ts []token) (string, error) {
	// Restrict the spelling independently of the SQL parser: quoted names,
	// dotted names and system variables must never enter generated SQL.
	if len(ts) < 5 || ts[1].raw != "@" || ts[1].end != ts[2].start || !nativeUserVariableName.MatchString(ts[2].raw) {
		return "", fmt.Errorf("会话变量赋值仅支持 1–64 字节的裸 ASCII 变量名，不支持系统变量")
	}
	op := 3
	if ts[op].raw == ":" {
		op++
	}
	if op >= len(ts)-1 || ts[op].raw != "=" {
		return "", fmt.Errorf("会话变量赋值仅支持 SET @变量 = 整数表达式")
	}
	n, err := c.parse(c.info.Body[ts[0].start:ts[len(ts)-1].end])
	if err != nil {
		return "", err
	}
	stmt, ok := n.(*ast.SetStmt)
	if !ok || len(stmt.Variables) != 1 {
		return "", fmt.Errorf("会话变量赋值一次只能设置一个变量")
	}
	v := stmt.Variables[0]
	if v.IsSystem || v.IsGlobal || v.IsInstance || v.ExtendValue != nil || !strings.EqualFold(v.Name, ts[2].raw) {
		return "", fmt.Errorf("会话变量赋值不支持系统变量或特殊赋值")
	}
	if !c.opt.NativeUserVariables {
		return "", ErrNativeUserVariablesRequired
	}
	rhs, err := c.nativeIntegerExpression(v.Value)
	if err != nil {
		return "", err
	}
	c.nativeUserVariables = true
	// User variable names have their own case-insensitive namespace and must
	// never follow the task's table/column case mapping.
	return "PERFORM @" + ts[2].raw + " := (" + rhs + ");\n", nil
}

// nativeIntegerExpression deliberately does not reuse the general expression
// generator: changing its arithmetic casts would alter frozen v1–v3 plans.
func (c *converter) nativeIntegerExpression(n ast.ExprNode) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("会话变量赋值仅支持有符号整数列、int64 常量、NULL 和整数加减乘，需要人工适配")
	}
	switch e := n.(type) {
	case ast.ValueExpr:
		if mysql.HasIsBooleanFlag(e.GetType().GetFlag()) {
			return bad()
		}
		switch v := e.GetValue().(type) {
		case nil:
			return "CAST(NULL AS bigint)", nil
		case int64:
			return "CAST(" + strconv.FormatInt(v, 10) + " AS bigint)", nil
		case uint64:
			if v <= math.MaxInt64 {
				return "CAST(" + strconv.FormatUint(v, 10) + " AS bigint)", nil
			}
		}
	case *ast.ColumnNameExpr:
		if !strings.EqualFold(e.Name.Table.O, "NEW") && !strings.EqualFold(e.Name.Table.O, "OLD") {
			return bad()
		}
		v, err := c.expr(e, nil)
		if err != nil {
			return "", err
		}
		t := c.tables[c.info.TableName]
		col, err := c.column(t, e.Name.Name.O)
		if err != nil {
			return "", err
		}
		if err = c.nativeSignedIntegerColumn(t, col); err != nil {
			return "", err
		}
		return "CAST(" + v.sql + " AS bigint)", nil
	case *ast.ParenthesesExpr:
		v, err := c.nativeIntegerExpression(e.Expr)
		return "(" + v + ")", err
	case *ast.UnaryOperationExpr:
		if e.Op != opcode.Plus && e.Op != opcode.Minus {
			return bad()
		}
		// The parser represents the magnitude of MIN_INT64 as uint64. Cast
		// the complete negative literal, rather than overflowing its magnitude.
		if e.Op == opcode.Minus {
			if literal, ok := e.V.(ast.ValueExpr); ok {
				if v, ok := literal.GetValue().(uint64); ok && v == uint64(math.MaxInt64)+1 {
					return "CAST('-9223372036854775808' AS bigint)", nil
				}
			}
		}
		v, err := c.nativeIntegerExpression(e.V)
		if err != nil {
			return "", err
		}
		op := "+"
		if e.Op == opcode.Minus {
			// MySQL can negate MIN_INT64 into an unsigned result, while PG
			// bigint negation overflows. Accept only operands whose successful
			// integer range is provably above MIN_INT64.
			if !c.nativeCanNegateInteger(e.V) {
				return "", fmt.Errorf("会话变量的一元负号操作数可能为 MIN_INT64，MySQL 与目标 bigint 取负语义不同，需要人工适配")
			}
			op = "-"
		}
		return "(" + op + v + ")", nil
	case *ast.BinaryOperationExpr:
		op := map[opcode.Op]string{opcode.Plus: "+", opcode.Minus: "-", opcode.Mul: "*"}[e.Op]
		if op == "" {
			return bad()
		}
		a, err := c.nativeIntegerExpression(e.L)
		if err != nil {
			return "", err
		}
		b, err := c.nativeIntegerExpression(e.R)
		if err != nil {
			return "", err
		}
		return "(" + a + " " + op + " " + b + ")", nil
	}
	return bad()
}

func (c *converter) nativeCanNegateInteger(n ast.ExprNode) bool {
	if nativeIntegerAlwaysNull(n) {
		return true
	}
	lo, _ := c.nativeIntegerBounds(n)
	return lo != nil && lo.Cmp(big.NewInt(math.MinInt64)) > 0
}

// These bounds describe successful signed-bigint results, not possible NULLs
// or overflowing executions. They are used only after the strict expression
// converter has validated the complete operand and its column snapshots.
func (c *converter) nativeIntegerBounds(n ast.ExprNode) (*big.Int, *big.Int) {
	rangeOf := func(lo, hi int64) (*big.Int, *big.Int) { return big.NewInt(lo), big.NewInt(hi) }
	switch e := n.(type) {
	case ast.ValueExpr:
		switch v := e.GetValue().(type) {
		case int64:
			return rangeOf(v, v)
		case uint64:
			if v <= math.MaxInt64 {
				return rangeOf(int64(v), int64(v))
			}
		}
	case *ast.ColumnNameExpr:
		col, err := c.column(c.tables[c.info.TableName], e.Name.Name.O)
		if err != nil {
			return nil, nil
		}
		base := strings.ToLower(strings.TrimSpace(col.DataType))
		if i := strings.IndexAny(base, "( "); i >= 0 {
			base = base[:i]
		}
		switch base {
		case "tinyint":
			return rangeOf(-128, 127)
		case "smallint":
			return rangeOf(-32768, 32767)
		case "mediumint":
			return rangeOf(-8388608, 8388607)
		case "int", "integer":
			return rangeOf(math.MinInt32, math.MaxInt32)
		case "bigint":
			return rangeOf(math.MinInt64, math.MaxInt64)
		}
	case *ast.ParenthesesExpr:
		return c.nativeIntegerBounds(e.Expr)
	case *ast.UnaryOperationExpr:
		if e.Op == opcode.Minus {
			if literal, ok := e.V.(ast.ValueExpr); ok {
				if v, ok := literal.GetValue().(uint64); ok && v == uint64(math.MaxInt64)+1 {
					return rangeOf(math.MinInt64, math.MinInt64)
				}
			}
		}
		lo, hi := c.nativeIntegerBounds(e.V)
		if lo == nil {
			return nil, nil
		}
		if e.Op == opcode.Plus {
			return lo, hi
		}
		if e.Op == opcode.Minus {
			return new(big.Int).Neg(hi), new(big.Int).Neg(lo)
		}
	case *ast.BinaryOperationExpr:
		loA, hiA := c.nativeIntegerBounds(e.L)
		loB, hiB := c.nativeIntegerBounds(e.R)
		if loA == nil || loB == nil {
			return nil, nil
		}
		var lo, hi *big.Int
		switch e.Op {
		case opcode.Plus:
			lo, hi = new(big.Int).Add(loA, loB), new(big.Int).Add(hiA, hiB)
		case opcode.Minus:
			lo, hi = new(big.Int).Sub(loA, hiB), new(big.Int).Sub(hiA, loB)
		case opcode.Mul:
			products := []*big.Int{
				new(big.Int).Mul(loA, loB), new(big.Int).Mul(loA, hiB),
				new(big.Int).Mul(hiA, loB), new(big.Int).Mul(hiA, hiB),
			}
			lo, hi = products[0], products[0]
			for _, value := range products[1:] {
				if value.Cmp(lo) < 0 {
					lo = value
				}
				if value.Cmp(hi) > 0 {
					hi = value
				}
			}
		default:
			return nil, nil
		}
		min, max := big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)
		if lo.Cmp(min) < 0 {
			lo = min
		}
		if hi.Cmp(max) > 0 {
			hi = max
		}
		if lo.Cmp(hi) <= 0 {
			return lo, hi
		}
	}
	return nil, nil
}

func nativeIntegerAlwaysNull(n ast.ExprNode) bool {
	switch e := n.(type) {
	case ast.ValueExpr:
		return e.GetValue() == nil
	case *ast.ParenthesesExpr:
		return nativeIntegerAlwaysNull(e.Expr)
	case *ast.UnaryOperationExpr:
		return nativeIntegerAlwaysNull(e.V)
	case *ast.BinaryOperationExpr:
		return nativeIntegerAlwaysNull(e.L) || nativeIntegerAlwaysNull(e.R)
	}
	return false
}

func (c *converter) nativeSignedIntegerColumn(t *source.TableDDLInfo, col *source.ColumnInfo) error {
	m := c.opt.CharacterMetadata
	if m == nil {
		return sourceMetadataRequired("会话变量赋值的整数列 %s.%s 缺少源 COLUMN_TYPE 快照", t.TableName, col.Name)
	}
	var found *source.TriggerNumericColumn
	for i := range m.NumericColumns {
		v := &m.NumericColumns[i]
		if v.Schema == c.info.Schema && v.Table == t.TableName && v.Name == col.Name {
			if found != nil {
				return fmt.Errorf("会话变量赋值的整数列 %s.%s 的类型快照重复", t.TableName, col.Name)
			}
			found = v
		}
	}
	if found == nil {
		return sourceMetadataRequired("会话变量赋值的整数列 %s.%s 缺少源 COLUMN_TYPE 快照", t.TableName, col.Name)
	}
	for _, v := range m.Columns {
		if v.Schema == found.Schema && v.Table == found.Table && v.Name == found.Name {
			return fmt.Errorf("会话变量赋值的整数列 %s.%s 存在冲突的字符快照", t.TableName, col.Name)
		}
	}
	check := &source.TriggerCharacterMetadata{ServerVersion: m.ServerVersion, NumericColumns: []source.TriggerNumericColumn{*found}}
	if err := source.ValidateTriggerCharacterMetadata(check); err != nil {
		return fmt.Errorf("会话变量赋值的源整数类型快照不可用: %w", err)
	}
	parts := nativeSignedIntegerType.FindStringSubmatch(found.ColumnType)
	base := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.IndexAny(s, "( "); i >= 0 {
			s = s[:i]
		}
		if s == "integer" {
			return "int"
		}
		return s
	}
	if len(parts) == 0 || base(parts[1]) != base(col.DataType) {
		return fmt.Errorf("会话变量赋值仅支持已验证的有符号整数列，%s.%s 的类型 %s 需要人工适配", t.TableName, col.Name, found.ColumnType)
	}
	c.numericColumns[characterKey(found.Schema, found.Table, found.Name)] = *found
	return nil
}
