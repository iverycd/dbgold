package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

func viewString(expr ast.ExprNode) (string, bool) {
	v, ok := expr.(ast.ValueExpr)
	if !ok {
		return "", false
	}
	s, ok := v.GetValue().(string)
	return s, ok
}

// Match the common MySQL empty-value filter before relocating its column.
// A character column can contain an empty value; a temporal column cannot.
func (r *mysqlViewRewriter) emptyComparison(n *ast.BinaryOperationExpr) (ast.ExprNode, error) {
	if n.Op != opcode.NE {
		return nil, nil
	}
	left, right := n.L, n.R
	if _, ok := viewString(left); ok {
		left, right = right, left
	}
	value, literal := viewString(right)
	if !literal {
		return nil, nil
	}
	fn, ok := left.(*ast.FuncCallExpr)
	if !ok || fn.Schema.O != "" || fn.FnName.L != "ifnull" || len(fn.Args) != 2 {
		return nil, nil
	}
	if s, ok := viewString(fn.Args[1]); !ok || s != "" {
		return nil, nil
	}
	col, ok := fn.Args[0].(*ast.ColumnNameExpr)
	if !ok {
		return nil, nil
	}
	// In DM Oracle compatibility mode the fallback '' is itself NULL.
	// A status literal with an alphanumeric character differs from an empty
	// string even under space-padding or punctuation-ignoring collations.
	if value != "" && !strings.ContainsFunc(value, func(c rune) bool {
		return unicode.IsLetter(c) || unicode.IsDigit(c)
	}) {
		return nil, fmt.Errorf("IFNULL(%s, '') 与 %q 比较需要核对源排序规则", col.Name.String(), value)
	}
	typ, err := r.columnType(col.Name)
	if err != nil {
		return nil, fmt.Errorf("判断 IFNULL(%s,'') 的列类型: %w", col.Name.String(), err)
	}
	r.mapQualifier(&col.Name.Schema, &col.Name.Table)
	var operand ast.ExprNode = col
	integer := false
	switch typ {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		integer = true
		if value != "" {
			// MySQL IFNULL(integer,'') promotes to text. '01' and '1' differ.
			ref, err := r.sql(col)
			if err != nil {
				return nil, err
			}
			operand = &mysqlViewSQL{col, "CAST(" + ref + " AS VARCHAR(32))"}
		}
	}
	var condition ast.ExprNode
	yes, no := int64(1), int64(0)
	switch {
	case value != "" && (mysqlViewTextType(typ) || integer):
		// A NULL source status passes <> '1', including in a SELECT projection.
		condition = &ast.BinaryOperationExpr{L: operand, Op: opcode.EQ, R: right}
		yes, no = 0, 1
	case value != "":
		return nil, fmt.Errorf("IFNULL(%s, '') 与 %q 比较需核对 %s 类型", col.Name.String(), value, typ)
	case mysqlViewTextType(typ):
		condition = &ast.IsNullExpr{Expr: &ast.FuncCallExpr{FnName: ast.NewCIStr("NULLIF"), Args: []ast.ExprNode{col, right}}, Not: true}
	case integer || typ == "datetime" || typ == "timestamp" || typ == "date" || typ == "time":
		condition = &ast.IsNullExpr{Expr: col, Not: true}
	default:
		return nil, fmt.Errorf("IFNULL(%s,'') <> '' 需核对 %s 类型与空串的比较", col.Name.String(), typ)
	}
	return &ast.CaseExpr{WhenClauses: []*ast.WhenClause{{Expr: condition, Result: ast.NewValueExpr(yes, "", "")}}, ElseClause: ast.NewValueExpr(no, "", "")}, nil
}

func mysqlViewTextType(typ string) bool {
	switch typ {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return true
	}
	return false
}

func (r *mysqlViewRewriter) columnType(col *ast.ColumnName) (string, error) {
	for i := len(r.scopes) - 1; i >= 0; i-- {
		var found string
		for alias, ref := range r.scopes[i].relations {
			if col.Table.O != "" && alias != col.Table.L {
				continue
			}
			if ref.table == "" || ref.schema != "" && ref.schema != r.m.dmSourceSchema || ref.schema == "" && r.isCTE(ast.NewCIStr(ref.table)) {
				return "", fmt.Errorf("无法解析 %s 的源关系", col.String())
			}
			info, err := r.m.sourceTableDDLInfo(r.ctx, ref.table)
			if err != nil {
				return "", err
			}
			if info == nil {
				return "", fmt.Errorf("缺少源表 %s 元数据", ref.table)
			}
			for _, c := range info.Columns {
				if strings.EqualFold(c.Name, col.Name.O) {
					if found != "" {
						return "", fmt.Errorf("列 %s 存在多个来源", col.String())
					}
					found = strings.ToLower(c.DataType)
				}
			}
			if col.Table.O != "" && found == "" {
				return "", fmt.Errorf("源表 %s 缺少列 %s", ref.table, col.Name.O)
			}
		}
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("找不到源列 %s", col.String())
}

func (r *mysqlViewRewriter) damengExpr(expr ast.ExprNode) (string, error) {
	switch n := expr.(type) {
	case *ast.IsTruthExpr:
		// MySQL 8 may store NOT (x IN (...)) as (x IN (...)) IS FALSE.
		// CASE also preserves IS [NOT] TRUE/FALSE when the operand is NULL.
		test, err := r.sql(n.Expr)
		if err != nil {
			return "", err
		}
		if n.True == 0 {
			test = "NOT (" + test + ")"
		}
		if n.Not {
			return "(CASE WHEN " + test + " THEN 0 ELSE 1 END)", nil
		}
		return "(CASE WHEN " + test + " THEN 1 ELSE 0 END)", nil
	case *ast.AggregateFuncExpr:
		if strings.EqualFold(n.F, "group_concat") {
			return r.damengGroupConcat(n)
		}
	case *ast.FuncCastExpr:
		return r.damengCast(n)
	case *ast.FuncCallExpr:
		if n.Schema.O != "" {
			return "", nil
		}
		switch n.FnName.L {
		case "date_add", "date_sub", "adddate", "subdate":
			if len(n.Args) != 3 {
				return "", fmt.Errorf("%s 日期区间参数不完整", n.FnName.O)
			}
			unit, ok := n.Args[2].(*ast.TimeUnitExpr)
			if !ok {
				return "", fmt.Errorf("%s 未知日期单位", n.FnName.O)
			}
			switch unit.Unit {
			case ast.TimeUnitMicrosecond, ast.TimeUnitSecond, ast.TimeUnitMinute, ast.TimeUnitHour, ast.TimeUnitDay, ast.TimeUnitWeek, ast.TimeUnitMonth, ast.TimeUnitQuarter, ast.TimeUnitYear:
			default:
				return "", fmt.Errorf("%s 的复合区间 %s 需手工适配", n.FnName.O, unit.Unit)
			}
			date, err := r.sql(n.Args[0])
			if err != nil {
				return "", err
			}
			count, err := r.sql(n.Args[1])
			if err != nil {
				return "", err
			}
			if n.FnName.L == "date_sub" || n.FnName.L == "subdate" {
				count = "-(" + count + ")"
			}
			return "DATEADD(" + unit.Unit.String() + ", " + count + ", " + date + ")", nil
		case "uuid":
			if p, ok := r.m.writer.(interface {
				SupportsViewUUID(context.Context) (bool, error)
			}); ok && r.nativeUUID == nil {
				native, err := p.SupportsViewUUID(r.ctx)
				if err != nil {
					return "", err
				}
				r.nativeUUID = &native
			}
			if r.nativeUUID != nil && !*r.nativeUUID {
				return "LOWER(NEWID())", nil
			}
		case "convert":
			// CONVERT(expr USING charset) is distinct from the type-cast AST.
			if len(n.Args) != 2 {
				return "", fmt.Errorf("CONVERT 缺少字符集参数")
			}
			cs, _ := viewString(n.Args[1])
			if !strings.EqualFold(cs, "utf8mb4") {
				return "", fmt.Errorf("CONVERT USING %s 字符集需要手工适配", cs)
			}
			if _, ok := viewString(n.Args[0]); ok {
				return r.sql(n.Args[0])
			}
			if c, ok := n.Args[0].(*ast.ColumnNameExpr); ok {
				typ, err := r.columnType(c.Name)
				if err != nil {
					return "", err
				}
				if mysqlViewTextType(typ) {
					return r.sql(c)
				}
			}
			return "", fmt.Errorf("CONVERT USING utf8mb4 的输入不是已确认的文本，需核对源编码")
		}
	}
	return "", nil
}

func (r *mysqlViewRewriter) damengGroupConcat(n *ast.AggregateFuncExpr) (string, error) {
	if len(n.Args) != 2 {
		original, _ := r.sql(n)
		return "", fmt.Errorf("多表达式拼接的 NULL 语义需要手工适配: %s", original)
	}
	value, err := r.sql(n.Args[0])
	if err != nil {
		return "", err
	}
	sep, err := r.sql(n.Args[1])
	if err != nil {
		return "", err
	}
	if n.Distinct {
		value = "DISTINCT " + value
	}
	result := "LISTAGG(" + value + ", " + sep + ")"
	if n.Order == nil {
		return result, nil
	}
	var keys []string
	for _, item := range n.Order.Items {
		key := item.Expr
		if pos, ok := key.(*ast.PositionExpr); ok {
			if pos.N != 1 {
				return "", fmt.Errorf("GROUP_CONCAT 的排序序号 %d 超出单表达式范围", pos.N)
			}
			key = n.Args[0]
		}
		sql, err := r.sql(key)
		if err != nil {
			return "", err
		}
		if item.Desc {
			sql += " DESC NULLS LAST"
		} else {
			sql += " ASC NULLS FIRST"
		}
		keys = append(keys, sql)
	}
	return result + " WITHIN GROUP (ORDER BY " + strings.Join(keys, ", ") + ")", nil
}

func (r *mysqlViewRewriter) damengCast(n *ast.FuncCastExpr) (string, error) {
	t := n.Tp
	var typ string
	switch t.GetType() {
	case mysql.TypeLonglong:
		if !mysql.HasUnsignedFlag(t.GetFlag()) {
			typ = "BIGINT"
		}
	case mysql.TypeNewDecimal:
		p, s := t.GetFlen(), t.GetDecimal()
		if p < 0 {
			p = 10
		}
		if s < 0 {
			s = 0
		}
		if p <= 38 {
			typ = fmt.Sprintf("DECIMAL(%d,%d)", p, s)
		}
	case mysql.TypeDate:
		// DM's Oracle mode DATE carries hours, minutes and seconds.
		arg, err := r.sql(n.Expr)
		if err != nil {
			return "", err
		}
		return "CAST(TRUNC(CAST(" + arg + " AS TIMESTAMP)) AS DATE)", nil
	case mysql.TypeDatetime:
		typ = fmt.Sprintf("TIMESTAMP(%d)", max(t.GetDecimal(), 0))
	case mysql.TypeString, mysql.TypeVarString:
		if t.GetCharset() != "binary" && (!n.ExplicitCharSet || t.GetCharset() == "utf8mb4") {
			if t.GetFlen() > 0 {
				typ = fmt.Sprintf("VARCHAR(%d CHAR)", t.GetFlen())
			} else if t.GetFlen() < 0 {
				typ = "VARCHAR"
			}
		}
	}
	if typ == "" {
		orig, _ := r.sql(n)
		return "", fmt.Errorf("达梦需要手工适配类型转换 %s", orig)
	}
	arg, err := r.sql(n.Expr)
	if err != nil {
		return "", err
	}
	return "CAST(" + arg + " AS " + typ + ")", nil
}
