package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/valueconv"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

var mysqlCatalogExpressionUnescape = strings.NewReplacer(
	"\\\\", "\\", "\\'", "'", "\\0", "\x00", "\\n", "\n", "\\r", "\r", "\\Z", "\x1a",
)

func (m *Migrator) planMySQLPGUniqueConstraint(ctx context.Context, idx source.IndexInfo) source.IndexInfo {
	if idx.MySQLExpression == "" || !idx.IsUnique || idx.IsPrimary || !strings.EqualFold(idx.Method, "BTREE") ||
		m.cfg.MigrationPurpose == "cdc" ||
		m.reader.DBType() != "mysql" || !source.IsPGFamily(m.writer.DBType()) {
		return idx
	}
	first, second, ok := mysqlConditionalConcatColumns(idx.MySQLExpression)
	if !ok {
		return idx
	}
	info, err := m.sourceTableDDLInfo(ctx, idx.TableName)
	if err != nil {
		m.stopMigrationTask(ctx, err)
		idx.Unsupported = fmt.Sprintf("无法读取源表列定义以转换 MySQL 表达式索引: %v", err)
		return idx
	}
	if info == nil {
		return idx
	}
	a, b := mysqlIndexSourceColumn(info.Columns, first), mysqlIndexSourceColumn(info.Columns, second)
	if a == nil || b == nil || m.targetColumn(idx.TableName, a.Name) == m.targetColumn(idx.TableName, b.Name) {
		return idx
	}
	switch strings.ToLower(b.DataType) {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
	default:
		return idx
	}
	idx.Columns = []string{a.Name, b.Name}
	idx.PGUniqueConstraint = true
	idx.MySQLExpression = ""
	idx.Unsupported = ""
	return idx
}

// Only the column names from this exact expression shape enter the target
// plan. The source expression is catalog metadata, never executable target SQL.
func mysqlConditionalConcatColumns(expression string) (string, string, bool) {
	p := parser.New()
	node, err := p.ParseOneStmt("SELECT ("+expression+")", "utf8mb4", "utf8mb4_bin")
	if err != nil && strings.Contains(expression, "\\'") {
		// MySQL 8.0 can expose the escaped data dictionary representation.
		// Unwrap one catalog escape layer before parsing the SQL expression.
		expression = mysqlCatalogExpressionUnescape.Replace(expression)
		node, err = p.ParseOneStmt("SELECT ("+expression+")", "utf8mb4", "utf8mb4_bin")
	}
	if err != nil {
		return "", "", false
	}
	query, ok := node.(*ast.SelectStmt)
	if !ok || query.Kind != ast.SelectStmtKindSelect || query.Fields == nil || len(query.Fields.Fields) != 1 ||
		query.From != nil || query.Where != nil || query.GroupBy != nil || query.Having != nil ||
		query.OrderBy != nil || query.Limit != nil || query.LockInfo != nil || query.SelectIntoOpt != nil || query.With != nil {
		return "", "", false
	}
	field := query.Fields.Fields[0]
	conditional, ok := unwrapMySQLIndexExpression(field.Expr).(*ast.CaseExpr)
	if !ok || field.AsName.O != "" || conditional.Value != nil || len(conditional.WhenClauses) != 1 {
		return "", "", false
	}
	if conditional.ElseClause != nil {
		value, ok := unwrapMySQLIndexExpression(conditional.ElseClause).(ast.ValueExpr)
		if !ok || value.GetValue() != nil {
			return "", "", false
		}
	}
	clause := conditional.WhenClauses[0]
	comparison, ok := unwrapMySQLIndexExpression(clause.Expr).(*ast.BinaryOperationExpr)
	if !ok || comparison.Op != opcode.NE || !mysqlIndexString(comparison.R, "") {
		return "", "", false
	}
	call, ok := unwrapMySQLIndexExpression(clause.Result).(*ast.FuncCallExpr)
	if !ok || call.Schema.O != "" || call.FnName.L != "concat" || len(call.Args) != 3 || !mysqlIndexString(call.Args[1], ":") {
		return "", "", false
	}
	first, second := mysqlIndexColumn(call.Args[0]), mysqlIndexColumn(call.Args[2])
	tested := mysqlIndexColumn(comparison.L)
	return first, second, first != "" && second != "" && strings.EqualFold(tested, second) && !strings.EqualFold(first, second)
}

func unwrapMySQLIndexExpression(expr ast.ExprNode) ast.ExprNode {
	for {
		group, ok := expr.(*ast.ParenthesesExpr)
		if !ok {
			return expr
		}
		expr = group.Expr
	}
}

func mysqlIndexString(expr ast.ExprNode, want string) bool {
	value, ok := unwrapMySQLIndexExpression(expr).(ast.ValueExpr)
	if !ok {
		return false
	}
	str, ok := value.GetValue().(string)
	charset := value.GetType().GetCharset()
	return ok && str == want && (charset == "" || charset == "utf8mb4")
}

func mysqlIndexColumn(expr ast.ExprNode) string {
	ref, ok := unwrapMySQLIndexExpression(expr).(*ast.ColumnNameExpr)
	if !ok || ref.Name.Schema.O != "" || ref.Name.Table.O != "" {
		return ""
	}
	return ref.Name.Name.O
}

func mysqlIndexSourceColumn(columns []source.ColumnInfo, name string) *source.ColumnInfo {
	var match *source.ColumnInfo
	for i := range columns {
		col := &columns[i]
		if col.Name == name {
			return col
		}
		if strings.EqualFold(col.Name, name) {
			if match != nil {
				return nil
			}
			match = col
		}
	}
	return match
}

func (m *Migrator) recordMySQLPGUniqueConstraintWarning(report *MigrationReport, original, planned source.IndexInfo) {
	reason := fmt.Sprintf("索引 %s 已转换为普通联合唯一约束，按目标库规则逐列判重。目标库将空字符串作为普通值时，同一首列值下的重复空字符串会冲突；分隔符只作为列值的一部分。", original.IndexName)
	report.Warnings = append(report.Warnings, valueconv.MigrationWarning{
		Table: original.TableName, Column: strings.Join(planned.Columns, ", "), Reason: reason, Count: 1,
	})
	m.log.Warnf("表 %s: %s", original.TableName, reason)
}
