package mysqltrigger

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
)

func (c *converter) dmlTable(ref *ast.TableRefsClause) (*source.TableDDLInfo, error) {
	if ref == nil || ref.TableRefs == nil {
		return nil, fmt.Errorf("缺少 DML 目标表")
	}
	j := ref.TableRefs
	if j.Right != nil || j.On != nil || len(j.Using) > 0 || j.NaturalJoin || j.StraightJoin || j.ExplicitParens {
		return nil, fmt.Errorf("多表 DML 或 JOIN 需要人工适配")
	}
	ts, ok := j.Left.(*ast.TableSource)
	if !ok || ts.AsName.O != "" {
		return nil, fmt.Errorf("DML 表别名或派生表需要人工适配")
	}
	tn, ok := ts.Source.(*ast.TableName)
	if !ok || len(tn.IndexHints) > 0 || len(tn.PartitionNames) > 0 || tn.TableSample != nil || tn.AsOf != nil {
		return nil, fmt.Errorf("DML 表引用需要人工适配")
	}
	if tn.Name.O == c.info.TableName {
		return nil, fmt.Errorf("触发器修改所属表需要人工适配")
	}
	return c.table(tn.Schema.O, tn.Name.O)
}
func (c *converter) target(t *source.TableDDLInfo) string {
	return quote(c.opt.Schema) + "." + quote(c.opt.Name(t.TableName))
}
func (c *converter) dml(s string) (string, error) {
	n, err := c.parse(s)
	if err != nil {
		return "", err
	}
	switch n := n.(type) {
	case *ast.InsertStmt:
		if n.IsReplace || n.IgnoreErr || n.Setlist || n.Priority != mysql.NoPriority || len(n.OnDuplicate) > 0 || n.Select != nil || len(n.TableHints) > 0 || len(n.PartitionNames) > 0 || len(n.Columns) == 0 || len(n.Lists) == 0 {
			return "", fmt.Errorf("仅支持带明确列清单的 INSERT VALUES")
		}
		t, err := c.dmlTable(n.Table)
		if err != nil {
			return "", err
		}
		var columns []string
		var cols []*source.ColumnInfo
		seen := map[string]bool{}
		for _, cn := range n.Columns {
			if cn.Table.O != "" || cn.Schema.O != "" {
				return "", fmt.Errorf("INSERT 列名不支持表限定")
			}
			col, err := c.column(t, cn.Name.O)
			if err != nil {
				return "", err
			}
			if strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				return "", fmt.Errorf("显式写入依赖表自增列需要人工适配")
			}
			mapped := c.opt.Name(col.Name)
			if seen[mapped] {
				return "", fmt.Errorf("INSERT 重复列")
			}
			seen[mapped] = true
			columns = append(columns, quote(mapped))
			cols = append(cols, col)
		}
		for i := range t.Columns {
			col := &t.Columns[i]
			if !seen[c.opt.Name(col.Name)] && implicitExpression(col) {
				return "", fmt.Errorf("INSERT 省略列 %s 依赖源库表达式默认值或生成列，需要人工适配", col.Name)
			}
		}
		var values []string
		for _, list := range n.Lists {
			if len(list) != len(cols) {
				return "", fmt.Errorf("INSERT 列数和值数量不一致")
			}
			var parts []string
			for i, arg := range list {
				v, err := c.expr(arg, nil)
				if err != nil {
					return "", err
				}
				if v, err = assignColumn(cols[i], v); err != nil {
					return "", err
				}
				parts = append(parts, v.sql)
			}
			values = append(values, "("+strings.Join(parts, ", ")+")")
		}
		return "INSERT INTO " + c.target(t) + " (" + strings.Join(columns, ", ") + ") VALUES " + strings.Join(values, ", ") + ";\n", nil
	case *ast.UpdateStmt:
		if n.Order != nil || n.Limit != nil || n.Priority != mysql.NoPriority || n.IgnoreErr || n.MultipleTable || len(n.TableHints) > 0 || n.With != nil || len(n.List) == 0 {
			return "", fmt.Errorf("仅支持无 JOIN/LIMIT/IGNORE 的单表 UPDATE")
		}
		t, err := c.dmlTable(n.TableRefs)
		if err != nil {
			return "", err
		}
		var parts []string
		seen := map[string]bool{}
		for _, a := range n.List {
			if a.Column.Schema.O != "" || a.Column.Table.O != "" && a.Column.Table.O != t.TableName {
				return "", fmt.Errorf("UPDATE 列限定无效")
			}
			col, err := c.column(t, a.Column.Name.O)
			if err != nil {
				return "", err
			}
			if strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				return "", fmt.Errorf("更新依赖表自增列需要人工适配")
			}
			mapped := c.opt.Name(col.Name)
			if seen[mapped] {
				return "", fmt.Errorf("UPDATE 重复赋值列")
			}
			seen[mapped] = true
			v, err := c.expr(a.Expr, t)
			if err != nil {
				return "", err
			}
			if v, err = assignColumn(col, v); err != nil {
				return "", err
			}
			parts = append(parts, quote(mapped)+" = "+v.sql)
		}
		// MySQL evaluates assignments left-to-right whereas PG uses the old
		// row for every RHS. Restrict v1 to one assignment to avoid a mismatch.
		if len(parts) != 1 {
			return "", fmt.Errorf("多列 UPDATE 的赋值顺序需要人工适配")
		}
		for _, col := range t.Columns {
			if strings.Contains(strings.ToLower(col.Extra), "on update") && !seen[c.opt.Name(col.Name)] {
				return "", fmt.Errorf("UPDATE 依赖未显式赋值的 ON UPDATE 列 %s，需要人工适配", col.Name)
			}
		}
		where, err := c.where(n.Where, t)
		if err != nil {
			return "", err
		}
		return "UPDATE " + c.target(t) + " SET " + strings.Join(parts, ", ") + where + ";\n", nil
	case *ast.DeleteStmt:
		if n.Tables != nil || n.Order != nil || n.Limit != nil || n.Priority != mysql.NoPriority || n.IgnoreErr || n.Quick || n.IsMultiTable || n.BeforeFrom || len(n.TableHints) > 0 || n.With != nil {
			return "", fmt.Errorf("仅支持无 JOIN/LIMIT/IGNORE 的单表 DELETE")
		}
		t, err := c.dmlTable(n.TableRefs)
		if err != nil {
			return "", err
		}
		where, err := c.where(n.Where, t)
		if err != nil {
			return "", err
		}
		return "DELETE FROM " + c.target(t) + where + ";\n", nil
	}
	return "", fmt.Errorf("DML 语句需要人工适配")
}

func implicitExpression(col *source.ColumnInfo) bool {
	if generated(col) {
		return true
	}
	if col.Default == nil {
		return false
	}
	if strings.Contains(strings.ToUpper(col.Extra), "DEFAULT_GENERATED") {
		return true
	}
	// Before MySQL 8, temporal CURRENT_TIMESTAMP defaults are not always
	// marked DEFAULT_GENERATED. Character defaults with the same literal
	// spelling remain constants and must not be mistaken for functions.
	if kindOf(col.DataType) == kindTime {
		d := strings.ToUpper(strings.TrimSpace(*col.Default))
		for _, fn := range []string{"CURRENT_TIMESTAMP", "LOCALTIMESTAMP", "LOCALTIME", "NOW"} {
			if d == fn || strings.HasPrefix(d, fn+"(") {
				return true
			}
		}
	}
	return false
}
func (c *converter) where(n ast.ExprNode, t *source.TableDDLInfo) (string, error) {
	if n == nil {
		return "", nil
	}
	e, err := c.expr(n, t)
	if err != nil {
		return "", err
	}
	if e.kind != kindBool {
		return "", fmt.Errorf("WHERE 需要明确的布尔表达式")
	}
	return " WHERE " + e.sql, nil
}
