package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

// MySQL stores a SELECT body with resolved table names in information_schema.
// Parse that body once, before any identifier quoting or target-specific edits.
func (m *Migrator) prepareMySQLView(ctx context.Context, view source.ViewInfo) (source.ViewInfo, error) {
	view.ViewName = m.objName(view.ViewName)
	stmt, err := parser.New().ParseOneStmt(view.Definition, "utf8mb4", "utf8mb4_bin")
	if err != nil {
		return view, fmt.Errorf("解析 MySQL 视图 %s: %w", view.ViewName, err)
	}
	switch stmt.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
	default:
		return view, fmt.Errorf("MySQL 视图 %s 缺少 SELECT", view.ViewName)
	}
	r := mysqlViewRewriter{ctx: ctx, m: m, dm: m.writer.DBType() == "dameng"}
	r.flags = format.DefaultRestoreFlags | format.RestoreStringWithoutDefaultCharset | format.RestoreStringEscapeBackslash
	if r.dm {
		r.flags = format.RestoreStringSingleQuotes | format.RestoreStringWithoutCharset | format.RestoreKeyWordUppercase | format.RestoreNameDoubleQuotes | format.RestoreNameUppercase
	}
	r.flags |= format.RestoreSpacesAroundBinaryOperation | format.RestoreBracketAroundBinaryOperation
	if _, ok := stmt.Accept(&r); !ok {
		return view, r.err
	}
	if r.dm || r.changed {
		view.Definition, err = r.sql(stmt)
	}
	return view, err
}

type mysqlViewRelation struct {
	schema, table string
	alias         bool
}

type mysqlViewScope struct {
	relations map[string]mysqlViewRelation
	ctes      map[string]bool
}

type mysqlViewRewriter struct {
	ctx         context.Context
	m           *Migrator
	dm, changed bool
	flags       format.RestoreFlags
	scopes      []mysqlViewScope
	nativeUUID  *bool
	err         error
}

func (r *mysqlViewRewriter) sql(n ast.Node) (string, error) {
	var b strings.Builder
	err := n.Restore(format.NewRestoreCtx(r.flags, &b))
	return b.String(), err
}

func (r *mysqlViewRewriter) pushScope(from *ast.TableRefsClause) {
	s := mysqlViewScope{relations: map[string]mysqlViewRelation{}, ctes: map[string]bool{}}
	var collect func(ast.ResultSetNode)
	collect = func(node ast.ResultSetNode) {
		switch n := node.(type) {
		case *ast.Join:
			collect(n.Left)
			collect(n.Right)
		case *ast.TableSource:
			ref := mysqlViewRelation{alias: n.AsName.O != ""}
			key := n.AsName.L
			if t, ok := n.Source.(*ast.TableName); ok {
				ref.schema, ref.table = t.Schema.O, t.Name.O
				if key == "" {
					key = t.Name.L
				}
			}
			s.relations[key] = ref
		}
	}
	if from != nil {
		collect(from.TableRefs)
	}
	r.scopes = append(r.scopes, s)
}

func (r *mysqlViewRewriter) isCTE(name ast.CIStr) bool {
	for i := len(r.scopes) - 1; i >= 0; i-- {
		if r.scopes[i].ctes[name.L] {
			return true
		}
	}
	return false
}

func (r *mysqlViewRewriter) local(schema ast.CIStr) bool {
	if schema.O == "" || schema.O == r.m.dmSourceSchema {
		return true
	}
	for _, s := range r.m.cfg.StripViewSchemas {
		if strings.EqualFold(strings.TrimSpace(s), schema.O) {
			return true
		}
	}
	return false
}

func (r *mysqlViewRewriter) set(name *ast.CIStr, value string) {
	if name.O != value {
		*name = ast.NewCIStr(value)
		r.changed = true
	}
}

func (r *mysqlViewRewriter) relocate(schema *ast.CIStr) {
	if schema.O != "" && r.local(*schema) {
		r.set(schema, r.m.cfg.TargetSchema)
	}
}

func (r *mysqlViewRewriter) mapQualifier(schema, table *ast.CIStr) {
	if !r.local(*schema) {
		return
	}
	if schema.O == "" {
		for i := len(r.scopes) - 1; i >= 0; i-- {
			if ref, ok := r.scopes[i].relations[table.L]; ok {
				if ref.alias || !r.local(ast.NewCIStr(ref.schema)) {
					return
				}
				break
			}
		}
		if r.isCTE(*table) {
			return
		}
	}
	r.set(table, r.m.objName(table.O))
	r.relocate(schema)
}

func (r *mysqlViewRewriter) Enter(node ast.Node) (ast.Node, bool) {
	switch n := node.(type) {
	case *ast.SelectStmt:
		r.pushScope(n.From)
	case *ast.SetOprStmt:
		r.pushScope(nil)
	case *ast.CommonTableExpression:
		if n.IsRecursive {
			r.scopes[len(r.scopes)-1].ctes[n.Name.L] = true
		}
	case *ast.TableName:
		if r.local(n.Schema) && (n.Schema.O != "" || !r.isCTE(n.Name)) {
			r.set(&n.Name, r.m.objName(n.Name.O))
			if !r.dm && n.Schema.O == "" {
				r.set(&n.Schema, r.m.cfg.TargetSchema)
			} else {
				r.relocate(&n.Schema)
			}
		}
	case *ast.ColumnName:
		r.mapQualifier(&n.Schema, &n.Table)
	case *ast.SelectField:
		// SelectField.Accept does not visit WildCard.
		if n.WildCard != nil {
			r.mapQualifier(&n.WildCard.Schema, &n.WildCard.Table)
		}
	case *ast.FuncCallExpr:
		r.relocate(&n.Schema)
	case *ast.Join:
		if r.dm && n.Right != nil && n.Tp == ast.CrossJoin && n.On == nil && len(n.Using) == 0 && !n.NaturalJoin {
			// TiDB restores a cartesian product as JOIN without ON. DM requires a condition.
			one := ast.NewValueExpr(int64(1), "", "")
			n.On = &ast.OnCondition{Expr: &ast.BinaryOperationExpr{L: one, Op: opcode.EQ, R: one}}
		}
	case *ast.BinaryOperationExpr:
		if r.dm {
			next, err := r.emptyComparison(n)
			if err != nil {
				r.err = err
				return node, true
			}
			if next != nil {
				return next, true
			}
		}
	}
	return node, false
}

func (r *mysqlViewRewriter) Leave(node ast.Node) (ast.Node, bool) {
	if r.err != nil {
		return node, false
	}
	switch n := node.(type) {
	case *ast.CommonTableExpression:
		r.scopes[len(r.scopes)-1].ctes[n.Name.L] = true
	case *ast.SelectStmt, *ast.SetOprStmt:
		r.scopes = r.scopes[:len(r.scopes)-1]
	}
	if r.dm {
		if expr, ok := node.(ast.ExprNode); ok {
			sql, err := r.damengExpr(expr)
			if err != nil {
				r.err = err
				return node, false
			}
			if sql != "" {
				return &mysqlViewSQL{expr, sql}, true
			}
		}
	}
	return node, true
}

// A rendered DM expression can participate in the surrounding MySQL SELECT AST.
type mysqlViewSQL struct {
	ast.ExprNode
	text string
}

func (n *mysqlViewSQL) Restore(ctx *format.RestoreCtx) error { ctx.WritePlain(n.text); return nil }
