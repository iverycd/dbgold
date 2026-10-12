package tsqldameng

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnames"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
)

// MappedColumn describes a source relation's logical output and actual DM name.
type MappedColumn struct{ Source, Target string }
type MappedViewResult struct {
	SQL          string
	Dependencies []string
	Columns      []MappedColumn
}

type mappedScope struct {
	alias, table string
	columns      []MappedColumn
}

func parseMappedView(raw string, sqlServer bool) (*ast.SelectStmt, error) {
	ts, err := tokenize(raw)
	if err != nil {
		return nil, err
	}
	if sqlServer {
		i := 0
		if len(ts) == 0 {
			return nil, fmt.Errorf("视图定义为空")
		}
		if keyword(ts[i], "CREATE") {
			i++
			if i+1 < len(ts) && keyword(ts[i], "OR") && keyword(ts[i+1], "ALTER") {
				i += 2
			}
		} else if keyword(ts[i], "ALTER") {
			i++
		} else {
			return nil, fmt.Errorf("视图缺少 CREATE/ALTER VIEW 头")
		}
		if i >= len(ts) || !keyword(ts[i], "VIEW") {
			return nil, fmt.Errorf("不支持的视图头")
		}
		i += 2
		if i < len(ts) && ts[i].text == "." {
			i += 2
		}
		if i >= len(ts) || !keyword(ts[i], "AS") {
			return nil, fmt.Errorf("暂不支持视图列清单或 WITH 选项")
		}
		ts = ts[i+1:]
	}
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.ident {
			parts = append(parts, "`"+strings.ReplaceAll(t.text, "`", "``")+"`")
			continue
		}
		switch strings.ToUpper(t.text) {
		case "TOP", "WITH", "APPLY", "PIVOT", "UNPIVOT", "COLLATE", "OVER", "CONVERT", "TRY_CONVERT", "CAST", "TRY_CAST", "ORDER", "LIMIT", "OFFSET", "INTO", "+", "-", "/", "%":
			return nil, fmt.Errorf("暂不支持自动同步的视图语法 %s", t.text)
		}
		parts = append(parts, t.text)
	}
	p := parser.New()
	p.SetSQLMode(mysql.ModeNoBackslashEscapes | mysql.ModeIgnoreSpace)
	n, err := p.ParseOneStmt(strings.Join(parts, " "), "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return nil, fmt.Errorf("视图 SELECT 解析失败: %w", err)
	}
	s, ok := n.(*ast.SelectStmt)
	if !ok {
		return nil, fmt.Errorf("暂不支持 UNION 或非 SELECT 视图的列名同步")
	}
	return s, nil
}

// MappedView rewrites only bound ColumnNameExpr nodes. Literal values and alias
// namespaces are never subject to text replacement. Unknown grammar fails closed.
func MappedView(raw, targetSchema, sourceSchema string, sqlServer bool, relations map[string][]MappedColumn) (MappedViewResult, error) {
	var out MappedViewResult
	s, err := parseMappedView(raw, sqlServer)
	if err != nil {
		return out, err
	}
	var scopes []mappedScope
	var bind func(ast.ResultSetNode) error
	bind = func(n ast.ResultSetNode) error {
		switch n := n.(type) {
		case *ast.Join:
			if err := bind(n.Left); err != nil {
				return err
			}
			if n.Right != nil {
				return bind(n.Right)
			}
			return nil
		case *ast.TableSource:
			t, ok := n.Source.(*ast.TableName)
			if !ok {
				return fmt.Errorf("暂不支持子查询表源的列名同步")
			}
			if t.Schema.O != "" && !strings.EqualFold(t.Schema.O, sourceSchema) {
				return fmt.Errorf("无法确认跨 schema 引用 %s.%s", t.Schema.O, t.Name.O)
			}
			name := strings.ToUpper(t.Name.O)
			out.Dependencies = append(out.Dependencies, t.Name.O)
			alias := t.Name.O
			if n.AsName.O != "" {
				alias = n.AsName.O
			}
			for _, prev := range scopes {
				if strings.EqualFold(prev.alias, alias) {
					return fmt.Errorf("重复表别名 %s", alias)
				}
			}
			scopes = append(scopes, mappedScope{alias: alias, table: t.Name.O, columns: relations[name]})
			// The existing renderer accepts dbo and relocates it to targetSchema.
			t.Schema = ast.NewCIStr("")
			return nil
		default:
			return fmt.Errorf("无法识别视图表源 %T", n)
		}
	}
	if s.From != nil {
		if err = bind(s.From.TableRefs); err != nil {
			return out, err
		}
	}
	for _, scope := range scopes {
		if len(scope.columns) == 0 {
			return out, fmt.Errorf("依赖对象 %s 的列映射不可用", scope.table)
		}
	}
	var fields []*ast.SelectField
	for _, f := range s.Fields.Fields {
		if f.WildCard == nil {
			fields = append(fields, f)
			continue
		}
		matched := false
		if f.WildCard.Schema.O != "" {
			return out, fmt.Errorf("暂不支持三段式通配列")
		}
		for _, scope := range scopes {
			if f.WildCard.Table.O != "" && !strings.EqualFold(f.WildCard.Table.O, scope.alias) {
				continue
			}
			matched = true
			for _, c := range scope.columns {
				fields = append(fields, &ast.SelectField{Expr: &ast.ColumnNameExpr{Name: &ast.ColumnName{Table: ast.NewCIStr(scope.alias), Name: ast.NewCIStr(c.Source)}}})
			}
		}
		if !matched {
			return out, fmt.Errorf("通配列无法确定所属表")
		}
	}
	s.Fields.Fields = fields
	outputNames := make([]string, len(fields))
	for i, f := range fields {
		outputNames[i] = f.AsName.O
		if outputNames[i] == "" {
			if col, ok := f.Expr.(*ast.ColumnNameExpr); ok {
				outputNames[i] = col.Name.Name.O
			} else {
				return out, fmt.Errorf("视图表达式需显式列别名以同步依赖")
			}
		}
	}
	targets, err := damengnames.Plan(outputNames)
	if err != nil {
		return out, err
	}
	for i, f := range fields {
		if err := ValidName(targets[i]); err != nil {
			return out, err
		}
		f.AsName = ast.NewCIStr(targets[i])
		out.Columns = append(out.Columns, MappedColumn{Source: outputNames[i], Target: targets[i]})
	}
	v := &mappedColumnVisitor{scopes: scopes, schema: sourceSchema}
	s.Accept(v)
	if v.err != nil {
		return out, v.err
	}
	r := &viewRenderer{schema: targetSchema, deps: map[string]bool{}}
	out.SQL = r.selectNode(s)
	return out, r.err
}

type mappedColumnVisitor struct {
	scopes []mappedScope
	schema string
	err    error
}

func (v *mappedColumnVisitor) Enter(n ast.Node) (ast.Node, bool) {
	c, ok := n.(*ast.ColumnNameExpr)
	if !ok {
		return n, false
	}
	if c.Name.Schema.O != "" && !strings.EqualFold(c.Name.Schema.O, v.schema) {
		v.err = fmt.Errorf("跨 schema 列引用无法同步")
		return n, true
	}
	var found *MappedColumn
	var alias string
	for _, s := range v.scopes {
		if c.Name.Table.O != "" && !strings.EqualFold(c.Name.Table.O, s.alias) {
			continue
		}
		for _, col := range s.columns {
			if !strings.EqualFold(c.Name.Name.O, col.Source) {
				continue
			}
			if found != nil {
				v.err = fmt.Errorf("列 %s 归属有歧义，未自动改写", c.Name.Name.O)
				return n, true
			}
			copy := col
			found, alias = &copy, s.alias
		}
	}
	if found == nil {
		v.err = fmt.Errorf("列 %s 无法确定来源，未自动改写", c.Name.Name.O)
		return n, true
	}
	c.Name.Name, c.Name.Table, c.Name.Schema = ast.NewCIStr(found.Target), ast.NewCIStr(alias), ast.NewCIStr("")
	return n, true
}
func (v *mappedColumnVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, v.err == nil }
