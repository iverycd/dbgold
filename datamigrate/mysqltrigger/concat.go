package mysqltrigger

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

func (c *converter) concat(fn *ast.FuncCallExpr, t *source.TableDDLInfo) (expression, error) {
	c.requiresTextSemantics = true
	if len(fn.Args) == 0 {
		return expression{}, fmt.Errorf("CONCAT 至少需要一个参数")
	}
	var parts []string
	for _, arg := range fn.Args {
		s, err := c.concatArgument(arg, t)
		if err != nil {
			return expression{}, err
		}
		parts = append(parts, s)
	}
	if len(parts) == 1 {
		return expression{parts[0], kindText}, nil
	}
	return expression{"(" + strings.Join(parts, " || ") + ")", kindText}, nil
}

func (c *converter) concatArgument(n ast.ExprNode, t *source.TableDDLInfo) (string, error) {
	v, err := c.expr(n, t)
	if err != nil {
		return "", err
	}
	switch v.kind {
	case kindNull:
		return "CAST(NULL AS text)", nil
	case kindText:
		// Cast individual CHAR operands before COALESCE so a character
		// fallback does not acquire bpchar's trailing-space conversion.
		if p, ok := n.(*ast.ParenthesesExpr); ok {
			return c.concatArgument(p.Expr, t)
		}
		if fn, ok := n.(*ast.FuncCallExpr); ok && (strings.EqualFold(fn.FnName.O, "ifnull") || strings.EqualFold(fn.FnName.O, "coalesce")) {
			var args []string
			for _, a := range fn.Args {
				s, err := c.concatArgument(a, t)
				if err != nil {
					return "", err
				}
				args = append(args, s)
			}
			return "COALESCE(" + strings.Join(args, ", ") + ")", nil
		}
		return "CAST(" + v.sql + " AS text)", nil
	case kindNumber:
		s, err := c.concatNumber(n, v.sql, t)
		if err != nil {
			return "", err
		}
		return "CAST(" + s + " AS text)", nil
	default:
		return "", fmt.Errorf("CONCAT 仅支持文本、NULL、直接整数或定点数列及数值常量；时间、浮点、二进制或其他隐式转换需要人工适配")
	}
}

func (c *converter) concatNumber(n ast.ExprNode, sqlText string, t *source.TableDDLInfo) (string, error) {
	if numericConstant(n) {
		return sqlText, nil
	}
	if p, ok := n.(*ast.ParenthesesExpr); ok {
		return c.concatNumber(p.Expr, sqlText, t)
	}
	if ref, ok := n.(*ast.ColumnNameExpr); ok {
		owner := t
		if strings.EqualFold(ref.Name.Table.O, "NEW") || strings.EqualFold(ref.Name.Table.O, "OLD") {
			owner = c.tables[c.info.TableName]
		}
		col, err := c.column(owner, ref.Name.Name.O)
		if err != nil {
			return "", err
		}
		if err = c.concatNumericColumn(owner, col); err != nil {
			return "", err
		}
		dt := strings.ToLower(strings.TrimSpace(col.DataType))
		if i := strings.IndexAny(dt, "( "); i >= 0 {
			dt = dt[:i]
		}
		switch dt {
		case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
			return sqlText, nil
		case "decimal", "numeric":
			if col.Precision <= 0 || col.Precision > 65 || col.Scale < 0 || col.Scale > 30 || col.Scale > col.Precision {
				return "", fmt.Errorf("CONCAT 定点数列 %s 缺少可靠 precision/scale", col.Name)
			}
			return fmt.Sprintf("CAST(%s AS numeric(%d,%d))", sqlText, col.Precision, col.Scale), nil
		}
	}
	return "", fmt.Errorf("CONCAT 复合数值表达式的字符串格式无法保真，需要人工适配")
}

var concatNumericType = regexp.MustCompile(`(?i)^(tinyint|smallint|mediumint|int|integer|bigint|decimal|numeric)(?:\(([0-9]+)(?:,([0-9]+))?\))?(?: unsigned)?(?: zerofill)?$`)

// Integer display width only changes CONCAT output together with ZEROFILL.
// COLUMN_TYPE is trigger-specific metadata because the ordinary type snapshot
// deliberately omits these MySQL formatting flags.
func (c *converter) concatNumericColumn(t *source.TableDDLInfo, col *source.ColumnInfo) error {
	m := c.opt.CharacterMetadata
	if m == nil {
		return fmt.Errorf("CONCAT 数值列 %s.%s 缺少源 COLUMN_TYPE 格式快照", t.TableName, col.Name)
	}
	var found *source.TriggerNumericColumn
	for i := range m.NumericColumns {
		v := &m.NumericColumns[i]
		if v.Schema == c.info.Schema && v.Table == t.TableName && v.Name == col.Name {
			if found != nil {
				return fmt.Errorf("CONCAT 数值列 %s.%s 的格式快照重复", t.TableName, col.Name)
			}
			found = v
		}
	}
	if found == nil {
		return fmt.Errorf("CONCAT 数值列 %s.%s 缺少源 COLUMN_TYPE 格式快照", t.TableName, col.Name)
	}
	for _, v := range m.Columns {
		if v.Schema == found.Schema && v.Table == found.Table && v.Name == found.Name {
			return fmt.Errorf("CONCAT 数值列 %s.%s 存在冲突的字符列快照", t.TableName, col.Name)
		}
	}
	check := &source.TriggerCharacterMetadata{ServerVersion: m.ServerVersion, NumericColumns: []source.TriggerNumericColumn{*found}}
	if err := source.ValidateTriggerCharacterMetadata(check); err != nil {
		return fmt.Errorf("源数值列格式快照不可用: %w", err)
	}
	parts := concatNumericType.FindStringSubmatch(found.ColumnType)
	if len(parts) == 0 || strings.Contains(strings.ToLower(found.ColumnType), "zerofill") {
		return fmt.Errorf("CONCAT 数值列 %s.%s 的格式 %s 需要人工适配", t.TableName, col.Name, found.ColumnType)
	}
	base := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.IndexAny(s, "( "); i >= 0 {
			s = s[:i]
		}
		if s == "integer" {
			s = "int"
		}
		if s == "numeric" {
			s = "decimal"
		}
		return s
	}
	if base(parts[1]) != base(col.DataType) {
		return fmt.Errorf("CONCAT 数值列 %s.%s 的格式快照与源列类型不一致", t.TableName, col.Name)
	}
	if base(parts[1]) == "decimal" {
		p, pErr := strconv.ParseInt(parts[2], 10, 64)
		s, sErr := strconv.ParseInt(parts[3], 10, 64)
		if pErr != nil || sErr != nil || p != col.Precision || s != col.Scale || p <= 0 || p > 65 || s < 0 || s > 30 || s > p {
			return fmt.Errorf("CONCAT 定点数列 %s.%s 的格式快照与源 precision/scale 不一致", t.TableName, col.Name)
		}
	} else if parts[3] != "" {
		return fmt.Errorf("CONCAT 整数列 %s.%s 的格式快照无效", t.TableName, col.Name)
	}
	c.numericColumns[characterKey(found.Schema, found.Table, found.Name)] = *found
	return nil
}

func numericConstant(n ast.ExprNode) bool {
	switch e := n.(type) {
	case ast.ValueExpr:
		return e.GetValue() != nil
	case *ast.ParenthesesExpr:
		return numericConstant(e.Expr)
	case *ast.UnaryOperationExpr:
		return (e.Op == opcode.Plus || e.Op == opcode.Minus) && numericConstant(e.V)
	}
	return false
}
