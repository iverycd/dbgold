package mysqltrigger

import (
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
)

func (c *converter) insertSelect(node ast.ResultSetNode, destination *source.TableDDLInfo, columns []string, cols []*source.ColumnInfo, original string) (string, error) {
	q, ok := node.(*ast.SelectStmt)
	if !ok || q.Kind != ast.SelectStmtKindSelect || q.Where == nil || q.Fields == nil || len(q.Fields.Fields) != len(cols) || q.GroupBy != nil || q.Having != nil || q.OrderBy != nil || q.Limit != nil || q.LockInfo != nil || q.With != nil || q.SelectIntoOpt != nil || q.AfterSetOperator != nil || len(q.WindowSpecs) > 0 || len(q.TableHints) > 0 || len(q.Lists) > 0 || q.Distinct || q.IsInBraces || q.WithBeforeBraces {
		return "", fmt.Errorf("INSERT SELECT 仅支持单表、明确表达式列和 WHERE；聚合、UNION、排序、LIMIT、锁定或子查询需要人工适配")
	}
	if opts := q.SelectStmtOpts; opts != nil && (opts.Distinct || opts.ExplicitAll || opts.SQLBigResult || opts.SQLBufferResult || !opts.SQLCache || opts.SQLSmallResult || opts.CalcFoundRows || opts.StraightJoin || opts.Priority != mysql.NoPriority || len(opts.TableHints) > 0) {
		return "", fmt.Errorf("INSERT SELECT 不支持查询修饰符")
	}
	mode, err := savedMode(c.info.SQLMode)
	if err != nil {
		return "", err
	}
	tokens, err := lex(original, mode.HasANSIQuotesMode(), mode.HasNoBackslashEscapesMode())
	if err != nil {
		return "", err
	}
	for i, tok := range tokens {
		if tok.word == "SELECT" && i+1 < len(tokens) {
			switch tokens[i+1].word {
			case "SQL_CACHE", "SQL_NO_CACHE", "ALL", "DISTINCT", "DISTINCTROW", "SQL_BIG_RESULT", "SQL_SMALL_RESULT", "SQL_BUFFER_RESULT", "SQL_CALC_FOUND_ROWS", "HIGH_PRIORITY", "STRAIGHT_JOIN":
				return "", fmt.Errorf("INSERT SELECT 不支持查询修饰符")
			}
		}
	}
	from, err := c.tableReference(q.From, true)
	if err != nil {
		return "", err
	}
	var fields []string
	for i, field := range q.Fields.Fields {
		if field == nil || field.WildCard != nil || field.Expr == nil || field.AsName.O != "" {
			return "", fmt.Errorf("INSERT SELECT 需要明确表达式列，不支持星号或别名")
		}
		v, err := c.expr(field.Expr, from)
		if err != nil {
			return "", err
		}
		v, err = assignColumn(cols[i], v)
		if err != nil {
			return "", err
		}
		fields = append(fields, v.sql)
	}
	where, err := c.where(q.Where, from)
	if err != nil {
		return "", err
	}
	return "INSERT INTO " + c.target(destination) + " (" + strings.Join(columns, ", ") + ") SELECT " + strings.Join(fields, ", ") + " FROM " + c.target(from) + where + ";\n", nil
}
