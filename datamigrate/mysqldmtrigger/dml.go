package mysqldmtrigger

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
)

func (c *converter) tableReference(ref *ast.TableRefsClause, read bool) (*source.TableDDLInfo, error) {
	if ref == nil || ref.TableRefs == nil {
		return nil, fmt.Errorf("缺少 SQL 表引用")
	}
	j := ref.TableRefs
	if j.Right != nil || j.On != nil || len(j.Using) > 0 || j.NaturalJoin || j.StraightJoin || j.ExplicitParens {
		return nil, fmt.Errorf("JOIN 或多表 DML 需要人工适配")
	}
	ts, ok := j.Left.(*ast.TableSource)
	if !ok || ts.AsName.O != "" {
		return nil, fmt.Errorf("表别名或派生表需要人工适配")
	}
	tn, ok := ts.Source.(*ast.TableName)
	if !ok || len(tn.IndexHints) > 0 || len(tn.PartitionNames) > 0 || tn.TableSample != nil || tn.AsOf != nil {
		return nil, fmt.Errorf("表引用需要人工适配")
	}
	if !read && tn.Name.O == c.info.TableName {
		return nil, fmt.Errorf("触发器不能直接修改所属表")
	}
	return c.table(tn.Schema.O, tn.Name.O)
}

func (c *converter) assigned(col *source.ColumnInfo, v expression) (string, error) {
	if e := assignable(kindOf(col.DataType), v.kind); e != nil {
		return "", e
	}
	if kindOf(col.DataType) == kindTime && v.kind != kindNull && (strings.EqualFold(col.DataType, "datetime") || strings.EqualFold(col.DataType, "timestamp")) {
		if col.DateTimePrecision < 0 || col.DateTimePrecision > 6 {
			return "", fmt.Errorf("列 %s 时间精度无效", col.Name)
		}
		return fmt.Sprintf("CAST(%s AS TIMESTAMP(%d))", v.sql, col.DateTimePrecision), nil
	}
	return v.sql, nil
}
func (c *converter) where(n ast.ExprNode, t *source.TableDDLInfo) (string, error) {
	if n == nil {
		return "", nil
	}
	v, e := c.expr(n, t)
	if e != nil {
		return "", e
	}
	pred, e := c.predicate(v)
	if e != nil {
		return "", e
	}
	return " WHERE " + pred, nil
}
func implicitExpression(col source.ColumnInfo) bool {
	return col.Generated != "" || strings.Contains(strings.ToUpper(col.Extra), "GENERATED") || col.Default != nil && (strings.Contains(strings.ToUpper(col.Extra), "DEFAULT_GENERATED") || (kindOf(col.DataType) == kindTime && (strings.Contains(strings.ToUpper(*col.Default), "CURRENT_TIMESTAMP") || strings.Contains(strings.ToUpper(*col.Default), "NOW("))))
}

func (c *converter) dml(s string) (string, error) {
	n, e := c.parse(s)
	if e != nil {
		return "", e
	}
	switch n := n.(type) {
	case *ast.InsertStmt:
		if n.IsReplace || n.IgnoreErr || n.Setlist || n.Priority != mysql.NoPriority || len(n.OnDuplicate) > 0 || len(n.TableHints) > 0 || len(n.PartitionNames) > 0 || len(n.Columns) == 0 {
			return "", fmt.Errorf("INSERT 只支持明确列清单的 VALUES/SELECT，不支持 IGNORE/REPLACE/冲突更新")
		}
		t, e := c.tableReference(n.Table, false)
		if e != nil {
			return "", e
		}
		var cols []*source.ColumnInfo
		var names []string
		seen := map[string]bool{}
		for _, cn := range n.Columns {
			if cn.Table.O != "" || cn.Schema.O != "" {
				return "", fmt.Errorf("INSERT 列不能带表限定")
			}
			col, e := c.column(t, cn.Name.O)
			if e != nil {
				return "", e
			}
			mapped := c.columnName(t.TableName, col.Name)
			if seen[mapped] {
				return "", fmt.Errorf("INSERT 重复列 %s", col.Name)
			}
			if strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				return "", fmt.Errorf("显式写入依赖表自增列需要人工适配")
			}
			seen[mapped] = true
			cols = append(cols, col)
			names = append(names, quote(mapped))
		}
		for _, col := range t.Columns {
			if !seen[c.columnName(t.TableName, col.Name)] && implicitExpression(col) {
				return "", fmt.Errorf("INSERT 省略列 %s 依赖生成列或表达式默认值", col.Name)
			}
		}
		prefix := "INSERT INTO " + c.target(t) + " (" + strings.Join(names, ", ") + ") "
		if n.Select != nil {
			if len(n.Lists) > 0 {
				return "", fmt.Errorf("INSERT SELECT 与 VALUES 混用")
			}
			q, ok := n.Select.(*ast.SelectStmt)
			if !ok {
				return "", fmt.Errorf("INSERT SELECT 不支持 UNION")
			}
			query, e := c.selectSQL(q, cols)
			if e != nil {
				return "", e
			}
			return prefix + query + ";\n", nil
		}
		if len(n.Lists) != 1 {
			return "", fmt.Errorf("多行 INSERT 的目标兼容语法需要人工适配")
		}
		list := n.Lists[0]
		if len(list) != len(cols) {
			return "", fmt.Errorf("INSERT 列数和值数量不一致")
		}
		var values []string
		for i, arg := range list {
			v, e := c.expr(arg, nil)
			if e != nil {
				return "", e
			}
			sql, e := c.assigned(cols[i], v)
			if e != nil {
				return "", e
			}
			values = append(values, sql)
		}
		return prefix + "VALUES (" + strings.Join(values, ", ") + ");\n", nil
	case *ast.UpdateStmt:
		if n.Order != nil || n.Limit != nil || n.Priority != mysql.NoPriority || n.IgnoreErr || n.MultipleTable || len(n.TableHints) > 0 || n.With != nil || len(n.List) != 1 {
			return "", fmt.Errorf("UPDATE 仅支持无 JOIN/LIMIT/IGNORE 的单表单列赋值，避免改变 MySQL 左到右赋值语义")
		}
		t, e := c.tableReference(n.TableRefs, false)
		if e != nil {
			return "", e
		}
		a := n.List[0]
		if a.Column.Schema.O != "" || a.Column.Table.O != "" && a.Column.Table.O != t.TableName {
			return "", fmt.Errorf("UPDATE 列限定无效")
		}
		col, e := c.column(t, a.Column.Name.O)
		if e != nil {
			return "", e
		}
		if strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
			return "", fmt.Errorf("不能自动更新依赖表自增列")
		}
		for _, other := range t.Columns {
			if strings.Contains(strings.ToLower(other.Extra), "on update") && !strings.EqualFold(other.Name, col.Name) {
				return "", fmt.Errorf("UPDATE 依赖未显式赋值的 ON UPDATE 列 %s", other.Name)
			}
		}
		v, e := c.expr(a.Expr, t)
		if e != nil {
			return "", e
		}
		value, e := c.assigned(col, v)
		if e != nil {
			return "", e
		}
		where, e := c.where(n.Where, t)
		if e != nil {
			return "", e
		}
		return "UPDATE " + c.target(t) + " SET " + quote(c.columnName(t.TableName, col.Name)) + " = " + value + where + ";\n", nil
	case *ast.DeleteStmt:
		if n.Tables != nil || n.Order != nil || n.Limit != nil || n.Priority != mysql.NoPriority || n.IgnoreErr || n.Quick || n.IsMultiTable || n.BeforeFrom || len(n.TableHints) > 0 || n.With != nil {
			return "", fmt.Errorf("DELETE 仅支持无 JOIN/LIMIT/IGNORE 的单表删除")
		}
		t, e := c.tableReference(n.TableRefs, false)
		if e != nil {
			return "", e
		}
		where, e := c.where(n.Where, t)
		if e != nil {
			return "", e
		}
		return "DELETE FROM " + c.target(t) + where + ";\n", nil
	default:
		return "", fmt.Errorf("不支持此 DML AST")
	}
}

func validateSelect(q *ast.SelectStmt) error {
	if q == nil || q.Kind != ast.SelectStmtKindSelect || q.Fields == nil || q.GroupBy != nil || q.Having != nil || q.OrderBy != nil || q.Limit != nil || q.LockInfo != nil || q.With != nil || q.SelectIntoOpt != nil || q.AfterSetOperator != nil || len(q.WindowSpecs) > 0 || len(q.TableHints) > 0 || len(q.Lists) > 0 || q.Distinct || q.IsInBraces || q.WithBeforeBraces {
		return fmt.Errorf("SELECT 只支持静态单表明确表达式列，不支持分组/UNION/排序/LIMIT/锁定/子查询")
	}
	if o := q.SelectStmtOpts; o != nil && (o.Distinct || o.ExplicitAll || o.SQLBigResult || o.SQLBufferResult || !o.SQLCache || o.SQLSmallResult || o.CalcFoundRows || o.StraightJoin || o.Priority != mysql.NoPriority || len(o.TableHints) > 0) {
		return fmt.Errorf("SELECT 查询修饰符需要人工适配")
	}
	return nil
}
func (c *converter) selectParts(q *ast.SelectStmt, cols []*source.ColumnInfo) ([]string, string, error) {
	if e := validateSelect(q); e != nil {
		return nil, "", e
	}
	if cols != nil && len(q.Fields.Fields) != len(cols) {
		return nil, "", fmt.Errorf("SELECT 列数与目标数量不一致")
	}
	var t *source.TableDDLInfo
	tail := " FROM DUAL"
	var e error
	if q.From != nil {
		t, e = c.tableReference(q.From, true)
		if e != nil {
			return nil, "", e
		}
		tail = " FROM " + c.target(t)
	}
	var fields []string
	previousAllow, previousAggregate, previousPlain := c.allowAggregate, c.hasAggregate, c.hasPlainQueryColumn
	c.allowAggregate, c.hasAggregate, c.hasPlainQueryColumn = true, false, false
	defer func() {
		c.allowAggregate, c.hasAggregate, c.hasPlainQueryColumn = previousAllow, previousAggregate, previousPlain
	}()
	for i, f := range q.Fields.Fields {
		if f == nil || f.WildCard != nil || f.Expr == nil || f.AsName.O != "" {
			return nil, "", fmt.Errorf("SELECT 必须列出明确表达式，不支持星号或列别名")
		}
		v, e := c.expr(f.Expr, t)
		if e != nil {
			return nil, "", e
		}
		sql := v.sql
		if cols != nil {
			sql, e = c.assigned(cols[i], v)
			if e != nil {
				return nil, "", e
			}
		}
		fields = append(fields, sql)
	}
	if c.hasAggregate && c.hasPlainQueryColumn {
		return nil, "", fmt.Errorf("无 GROUP BY 的聚合查询不能混合普通表列")
	}
	c.allowAggregate = false
	where, e := c.where(q.Where, t)
	if e != nil {
		return nil, "", e
	}
	return fields, tail + where, nil
}
func (c *converter) selectSQL(q *ast.SelectStmt, cols []*source.ColumnInfo) (string, error) {
	fields, tail, e := c.selectParts(q, cols)
	if e != nil {
		return "", e
	}
	return "SELECT " + strings.Join(fields, ", ") + tail, nil
}

func (c *converter) selectInto() (string, error) {
	start := c.pos
	qtext, e := c.until(";")
	if e != nil && c.pos == len(c.tokens) {
		qtext = c.info.Body[c.tokens[start].start:]
		e = nil
	}
	if e != nil {
		return "", e
	}
	end := c.pos
	// INTO local variables is not in TiDB's grammar. Remove exactly this one
	// top-level clause, then parse and validate every remaining query AST node.
	into := -1
	depth := 0
	for i := start; i < end; i++ {
		t := c.tokens[i]
		if t.raw == "(" {
			depth++
		}
		if t.raw == ")" {
			depth--
		}
		if depth == 0 && t.word == "INTO" {
			if into >= 0 {
				return "", fmt.Errorf("SELECT 存在多个 INTO")
			}
			into = i
		}
	}
	if into < 0 {
		return "", fmt.Errorf("触发器 SELECT 必须 INTO 已声明局部变量")
	}
	i := into + 1
	var vars []variable
	for {
		if i >= end {
			return "", fmt.Errorf("SELECT INTO 缺少变量")
		}
		name, e := identifier(c.tokens[i])
		if e != nil {
			return "", fmt.Errorf("SELECT INTO 仅支持局部变量")
		}
		v, ok := c.lookup(name)
		if !ok {
			return "", fmt.Errorf("SELECT INTO 变量 %s 未声明", name)
		}
		vars = append(vars, v)
		i++
		if i >= end || c.tokens[i].raw != "," {
			break
		}
		i++
	}
	query := c.info.Body[c.tokens[start].start:c.tokens[into].start]
	if i < end {
		query += c.info.Body[c.tokens[i].start:c.tokens[end-1].end]
	}
	n, e := c.parse(query)
	if e != nil {
		return "", e
	}
	q, ok := n.(*ast.SelectStmt)
	if !ok {
		return "", fmt.Errorf("SELECT INTO 不支持 UNION")
	}
	if e = validateSelect(q); e != nil {
		return "", e
	}
	if len(q.Fields.Fields) != len(vars) {
		return "", fmt.Errorf("SELECT INTO 变量数量不匹配")
	}
	var t *source.TableDDLInfo
	tail := " FROM DUAL"
	if q.From != nil {
		t, e = c.tableReference(q.From, true)
		if e != nil {
			return "", e
		}
		tail = " FROM " + c.target(t)
	}
	where, e := c.where(q.Where, t)
	if e != nil {
		return "", e
	}
	var declarations, assignments strings.Builder
	var fields, temps []string
	previousAllow, previousAggregate, previousPlain := c.allowAggregate, c.hasAggregate, c.hasPlainQueryColumn
	c.allowAggregate, c.hasAggregate, c.hasPlainQueryColumn = true, false, false
	defer func() {
		c.allowAggregate, c.hasAggregate, c.hasPlainQueryColumn = previousAllow, previousAggregate, previousPlain
	}()
	for j, f := range q.Fields.Fields {
		if f.WildCard != nil || f.Expr == nil || f.AsName.O != "" {
			return "", fmt.Errorf("SELECT INTO 不支持星号或别名")
		}
		v, e := c.expr(f.Expr, t)
		if e != nil {
			return "", e
		}
		if e = assignable(vars[j].kind, v.kind); e != nil {
			return "", e
		}
		tmp := quote(c.fresh("INTO"))
		declarations.WriteString(tmp + " " + vars[j].typ + ";\n")
		temps = append(temps, tmp)
		fields = append(fields, v.sql)
		assignments.WriteString(quote(vars[j].name) + " := " + tmp + ";\n")
	}
	if c.hasAggregate && c.hasPlainQueryColumn {
		return "", fmt.Errorf("无 GROUP BY 的聚合查询不能混合普通表列")
	}
	_ = qtext
	return "DECLARE\n" + declarations.String() + "BEGIN\nSELECT " + strings.Join(fields, ", ") + " INTO " + strings.Join(temps, ", ") + tail + where + ";\n" + assignments.String() + "EXCEPTION WHEN NO_DATA_FOUND THEN NULL;\nEND;\n", nil
}
