package sqlserverview

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

type viewValueKind int

const (
	viewUnknown viewValueKind = iota
	viewNumber
	viewBoolean
	viewDate
	viewTimestamp
	viewTime
	viewNull
	viewCondition
	viewStar
)

func (k viewValueKind) comparable() bool {
	switch k {
	case viewNumber, viewBoolean, viewDate, viewTimestamp, viewTime:
		return true
	default:
		return false
	}
}

type viewValue struct {
	sql    string
	kind   viewValueKind
	column *source.ColumnInfo
}

func (v viewValue) predicate() (string, error) {
	if v.kind == viewCondition {
		return v.sql, nil
	}
	if v.kind == viewBoolean {
		return "(" + v.sql + " = 1)", nil
	}
	return "", fmt.Errorf("条件须为比较表达式或布尔列")
}

func (q *foreignView) predicate(p *viewTokens, scope int, aggregate bool) (string, error) {
	v, err := q.expression(p, scope, 0, aggregate, false, 0)
	if err != nil {
		return "", err
	}
	return v.predicate()
}

func precedenceOf(t sqlsyntax.Token) int {
	switch {
	case t.Is("OR"):
		return 1
	case t.Is("AND"):
		return 2
	case keyword(t, "=", "!=", "<>", "<", ">", "<=", ">=", "IS", "BETWEEN", "IN", "NOT"):
		return 3
	default:
		return 0
	}
}

func (q *foreignView) expression(p *viewTokens, scope, minimum int, aggregate, wildcard bool, depth int) (viewValue, error) {
	if depth >= 128 {
		return viewValue{}, fmt.Errorf("视图表达式嵌套过深")
	}
	left, err := q.atom(p, scope, aggregate, wildcard, depth)
	if err != nil {
		return viewValue{}, err
	}
	for {
		operator := p.peek()
		precedence := precedenceOf(operator)
		if precedence == 0 || precedence < minimum {
			return left, nil
		}
		p.pos++
		switch {
		case operator.Is("IS"):
			left, err = q.isCondition(p, left)
		case keyword(operator, "IN", "BETWEEN", "NOT"):
			left, err = q.listCondition(p, left, operator, scope, aggregate, depth)
		default:
			var right viewValue
			right, err = q.expression(p, scope, precedence+1, aggregate, false, depth+1)
			if err == nil {
				left, err = binaryViewCondition(left, operator, right)
			}
		}
		if err != nil {
			return viewValue{}, err
		}
	}
}

func binaryViewCondition(left viewValue, op sqlsyntax.Token, right viewValue) (viewValue, error) {
	l, r := left.sql, right.sql
	if keyword(op, "AND", "OR") {
		var err error
		if l, err = left.predicate(); err != nil {
			return viewValue{}, err
		}
		if r, err = right.predicate(); err != nil {
			return viewValue{}, err
		}
	} else if !canCompareViewValues(left, right, keyword(op, "=", "!=", "<>")) {
		return viewValue{}, fmt.Errorf("比较 %s 的类型、隐式转换或字符排序规则需人工适配", op.Text)
	}
	return viewValue{sql: "(" + l + " " + strings.ToUpper(op.Text) + " " + r + ")", kind: viewCondition}, nil
}

func canCompareViewValues(left, right viewValue, equality bool) bool {
	if left.kind == viewNull || right.kind == viewNull {
		return left.kind != viewCondition && left.kind != viewStar && right.kind != viewCondition && right.kind != viewStar
	}
	return left.kind == right.kind && left.kind.comparable() && (equality || left.kind != viewBoolean)
}

func (q *foreignView) isCondition(p *viewTokens, value viewValue) (viewValue, error) {
	negated := p.take("NOT")
	not := ""
	if negated {
		not = "NOT "
	}
	if (value.kind != viewCondition && value.kind != viewStar && p.take("NULL")) ||
		(q.sourceType != "oracle" && value.kind == viewBoolean && p.take("UNKNOWN")) {
		return viewValue{sql: "(" + value.sql + " IS " + not + "NULL)", kind: viewCondition}, nil
	}
	if q.sourceType != "oracle" && value.kind == viewBoolean && keyword(p.peek(), "TRUE", "FALSE") {
		expected := "1"
		if p.peek().Is("FALSE") {
			expected = "0"
		}
		p.pos++
		comparison := "(" + value.sql + " = " + expected + ")"
		if negated {
			comparison = "(NOT " + comparison + " OR " + value.sql + " IS NULL)"
		} else {
			comparison = "(" + comparison + " AND " + value.sql + " IS NOT NULL)"
		}
		return viewValue{sql: comparison, kind: viewCondition}, nil
	}
	return viewValue{}, fmt.Errorf("不支持视图 IS %s%q 条件", not, p.peek().Text)
}

func (q *foreignView) listCondition(p *viewTokens, left viewValue, operator sqlsyntax.Token, scope int, aggregate bool, depth int) (viewValue, error) {
	negated := ""
	if operator.Is("NOT") {
		negated = "NOT "
		operator = p.peek()
		if !keyword(operator, "IN", "BETWEEN") {
			return viewValue{}, fmt.Errorf("不支持的 NOT %q 条件", operator.Text)
		}
		p.pos++
	}
	operand := func(equality bool) (string, error) {
		x, err := q.expression(p, scope, 4, aggregate, false, depth+1)
		if err != nil {
			return "", err
		}
		if !canCompareViewValues(left, x, equality) {
			return "", fmt.Errorf("%s 条件的类型或字符排序规则需人工适配", operator.Text)
		}
		return x.sql, nil
	}
	if operator.Is("BETWEEN") {
		low, err := operand(false)
		if err != nil {
			return viewValue{}, err
		}
		if err := p.expect("AND"); err != nil {
			return viewValue{}, err
		}
		high, err := operand(false)
		if err != nil {
			return viewValue{}, err
		}
		return viewValue{sql: "(" + left.sql + " " + negated + "BETWEEN " + low + " AND " + high + ")", kind: viewCondition}, nil
	}
	if err := p.expect("("); err != nil {
		return viewValue{}, err
	}
	var items []string
	for {
		x, err := operand(true)
		if err != nil {
			return viewValue{}, err
		}
		items = append(items, x)
		if !p.take(",") {
			break
		}
	}
	if err := p.expect(")"); err != nil {
		return viewValue{}, err
	}
	return viewValue{sql: "(" + left.sql + " " + negated + "IN (" + strings.Join(items, ", ") + "))", kind: viewCondition}, nil
}

func (q *foreignView) atom(p *viewTokens, scope int, aggregate, wildcard bool, depth int) (viewValue, error) {
	if p.take("(") {
		v, err := q.expression(p, scope, 0, aggregate, false, depth+1)
		if err != nil {
			return viewValue{}, err
		}
		return v, p.expect(")")
	}
	if p.take("NOT") {
		v, err := q.expression(p, scope, 3, aggregate, false, depth+1)
		if err != nil {
			return viewValue{}, err
		}
		predicate, err := v.predicate()
		return viewValue{sql: "(NOT " + predicate + ")", kind: viewCondition}, err
	}
	if wildcard && p.take("*") {
		if scope == 0 {
			return viewValue{}, fmt.Errorf("通配列缺少源表")
		}
		return viewValue{sql: "*", kind: viewStar}, nil
	}
	t := p.peek()
	sign := ""
	if keyword(t, "+", "-") {
		sign = t.Text
		p.pos++
		t = p.peek()
		if t.Kind != 'n' {
			return viewValue{}, fmt.Errorf("正负号仅支持数值常量")
		}
	}
	if t.Kind == 'n' {
		p.pos++
		if strings.ContainsAny(t.Text, "eE") || len(strings.ReplaceAll(t.Text, ".", "")) > 38 {
			return viewValue{}, fmt.Errorf("暂不支持指数或超过 38 位的数值常量 %q", t.Text)
		}
		return viewValue{sql: sign + t.Text, kind: viewNumber}, nil
	}
	if p.take("NULL") {
		return viewValue{sql: "NULL", kind: viewNull}, nil
	}
	if q.sourceType != "oracle" && keyword(t, "TRUE", "FALSE") {
		p.pos++
		result := "CAST(0 AS bit)"
		if t.Is("TRUE") {
			result = "CAST(1 AS bit)"
		}
		return viewValue{sql: result, kind: viewBoolean}, nil
	}
	if q.sourceType == "oracle" && t.Is("N") && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].Kind == 's' {
		p.pos++
		t = p.peek()
	}
	if t.Kind == 's' {
		p.pos++
		if strings.ContainsRune(t.Value, 0) || q.sourceType == "oracle" && !strings.HasPrefix(t.Text, "'") {
			return viewValue{}, fmt.Errorf("不支持该字符串常量")
		}
		if t.Value == "" && q.sourceType == "oracle" {
			return viewValue{sql: "CAST(NULL AS nvarchar(1))", kind: viewNull}, nil
		}
		return viewValue{sql: literal(t.Value), kind: viewUnknown}, nil
	}
	if t.Kind == 'i' && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].Is("(") {
		if !aggregate {
			return viewValue{}, fmt.Errorf("视图条件或函数参数中的函数 %q 需人工适配", t.Text)
		}
		return q.aggregate(p, scope, depth)
	}
	return q.reference(p, scope, wildcard)
}

func (q *foreignView) aggregate(p *viewTokens, scope, depth int) (viewValue, error) {
	t := p.peek()
	if !keyword(t, "COUNT", "MIN", "MAX", "SUM") {
		return viewValue{}, fmt.Errorf("视图函数或聚合 %q 需人工适配", t.Text)
	}
	p.pos += 2 // Function name and the opening parenthesis.
	distinct := p.take("DISTINCT")
	if !distinct {
		p.take("ALL")
	}
	if t.Is("COUNT") && !distinct && p.take("*") {
		return viewValue{sql: "COUNT_BIG(*)", kind: viewNumber}, p.expect(")")
	}
	argument, err := q.expression(p, scope, 0, false, false, depth+1)
	if err != nil {
		return viewValue{}, err
	}
	if err := p.expect(")"); err != nil {
		return viewValue{}, err
	}
	if argument.kind == viewCondition || distinct && !argument.kind.comparable() {
		return viewValue{}, fmt.Errorf("聚合 %q 的表达式类型或字符排序规则需人工适配", t.Text)
	}
	function, kind := strings.ToUpper(t.Text), argument.kind
	switch function {
	case "COUNT":
		function, kind = "COUNT_BIG", viewNumber
	case "MIN", "MAX":
		if !kind.comparable() || kind == viewBoolean {
			return viewValue{}, fmt.Errorf("聚合 %q 的列类型或字符排序规则需人工适配", t.Text)
		}
	case "SUM":
		if kind != viewNumber || argument.column == nil {
			return viewValue{}, fmt.Errorf("SUM 仅支持精度明确的数值列")
		}
		column := argument.column
		switch strings.ToLower(column.DataType) {
		case "int2", "int4", "smallint", "integer":
			argument.sql = "CAST(" + argument.sql + " AS bigint)"
		case "int8", "bigint":
			argument.sql = "CAST(" + argument.sql + " AS decimal(38,0))"
		default:
			if column.Precision <= 0 || column.Scale < 0 || column.Scale > 38 {
				return viewValue{}, fmt.Errorf("SUM 仅支持精度明确的数值列")
			}
			argument.sql = fmt.Sprintf("CAST(%s AS decimal(38,%d))", argument.sql, column.Scale)
		}
	}
	if distinct {
		argument.sql = "DISTINCT " + argument.sql
	}
	return viewValue{sql: function + "(" + argument.sql + ")", kind: kind}, nil
}

func viewColumnKind(column *source.ColumnInfo) viewValueKind {
	if column.TypeKind == "d" || column.TypeSchema != "" && column.TypeSchema != "pg_catalog" && column.TypeSchema != "sys_catalog" {
		return viewUnknown
	}
	typeName := strings.ToLower(column.DataType)
	if strings.HasPrefix(typeName, "timestamp(") && strings.HasSuffix(typeName, ")") {
		return viewTimestamp
	}
	switch typeName {
	case "int2", "int4", "int8", "smallint", "integer", "bigint", "int", "numeric", "decimal", "number":
		return viewNumber
	case "bool", "boolean":
		return viewBoolean
	case "date":
		return viewDate
	case "timestamp", "timestamp without time zone":
		return viewTimestamp
	case "time", "time without time zone":
		return viewTime
	default:
		return viewUnknown
	}
}

func (q *foreignView) reference(p *viewTokens, scope int, wildcard bool) (viewValue, error) {
	first, err := q.identifier(p)
	if err != nil {
		return viewValue{}, err
	}
	var qualifier string
	columnName := first
	if p.take(".") {
		qualifier = first
		if wildcard && p.take("*") {
			return q.qualifiedStar(qualifier, scope)
		}
		columnName, err = q.identifier(p)
		if err != nil {
			return viewValue{}, err
		}
		if p.take(".") {
			if qualifier != q.schema {
				return viewValue{}, fmt.Errorf("不支持引用迁移范围之外的 Schema %q", qualifier)
			}
			qualifier = columnName
			if wildcard && p.take("*") {
				return q.qualifiedStar(qualifier, scope)
			}
			columnName, err = q.identifier(p)
			if err != nil {
				return viewValue{}, err
			}
		}
	}
	var found viewValue
	for _, relation := range q.relations[:scope] {
		if qualifier != "" && qualifier != relation.alias {
			continue
		}
		for i := range q.tables[relation.name].Columns {
			c := &q.tables[relation.name].Columns[i]
			if c.Name != columnName {
				continue
			}
			if found.column != nil {
				return viewValue{}, fmt.Errorf("视图列 %q 存在歧义", columnName)
			}
			quoted, err := q.quote(columnName)
			if err != nil {
				return viewValue{}, err
			}
			found = viewValue{sql: relation.sqlAlias + "." + quoted, kind: viewColumnKind(c), column: c}
		}
	}
	if found.column == nil {
		return viewValue{}, fmt.Errorf("无法解析视图源列 %q", strings.TrimPrefix(qualifier+"."+columnName, "."))
	}
	return found, nil
}

func (q *foreignView) qualifiedStar(qualifier string, scope int) (viewValue, error) {
	for _, relation := range q.relations[:scope] {
		if relation.alias == qualifier {
			return viewValue{sql: relation.sqlAlias + ".*", kind: viewStar}, nil
		}
	}
	return viewValue{}, fmt.Errorf("无法解析通配列的表名 %q", qualifier)
}
