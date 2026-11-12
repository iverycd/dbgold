package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

func validPGFulltextIndex(idx source.IndexInfo) bool {
	return idx.PGFulltext && strings.EqualFold(idx.Method, "FULLTEXT") &&
		!idx.IsPrimary && !idx.IsUnique && idx.Unsupported == "" &&
		len(idx.Columns) != 0 && !slices.Contains(idx.Columns, "")
}

func createPGFulltextIndex(ctx context.Context, db *sql.DB, d dialect.Dialect, schema string, idx source.IndexInfo) error {
	name := dialect.IndexName(d, idx)
	if !validPGFulltextIndex(idx) || idx.TableName == "" || name == "" {
		return fmt.Errorf("PostgreSQL 全文索引缺少有效计划")
	}
	query := dialect.JoinSQL(d.IndexStatements(schema, idx))
	if query == "" {
		return fmt.Errorf("PostgreSQL 全文索引缺少创建语句")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	obj := repair.Object{Schema: schema, Table: idx.TableName, TargetName: name, Index: &idx}
	var verificationErr error
	_, err = execObservedAllow(ctx, conn, query, func(createErr error) bool {
		switch compatibilitySQLState(createErr) {
		case "42P07", "42710":
		default:
			return false
		}
		if obj.Schema == "" {
			const visibleSchema = `SELECT n.nspname FROM pg_catalog.pg_class t
JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
WHERE t.relname=$1 AND pg_catalog.pg_table_is_visible(t.oid)`
			if verificationErr = conn.QueryRowContext(ctx, visibleSchema, obj.Table).Scan(&obj.Schema); verificationErr != nil {
				return false
			}
		}
		result, err := inspectPGFulltextIndex(ctx, conn, obj)
		verificationErr = err
		if err == nil && result.Status != "succeeded" {
			verificationErr = fmt.Errorf("核验同名全文索引失败: %s", result.Message)
		}
		return verificationErr == nil
	})
	return errors.Join(err, verificationErr)
}

// IsGeneratedPGFulltextRepair checks whether the typed executor will issue the
// exact stored SQL for the bound object and unit.
func IsGeneratedPGFulltextRepair(d dialect.Dialect, obj repair.Object, units []repair.Unit) bool {
	idx := obj.Index
	if idx == nil || !validPGFulltextIndex(*idx) || !source.IsPGFamily(d.Name()) || obj.Kind != "index" || obj.Schema == "" ||
		obj.Table == "" || obj.Table != idx.TableName || obj.TargetName == "" ||
		obj.TargetName != dialect.IndexName(d, *idx) || len(units) != 1 || len(obj.Units) != 1 {
		return false
	}
	stmt := dialect.JoinSQL(d.IndexStatements(obj.Schema, *idx))
	unit, saved := units[0], obj.Units[0]
	return stmt != "" && unit.ID == saved.ID && unit.SQL == stmt && unit.OriginalSQL == stmt &&
		saved.SQL == stmt && saved.OriginalSQL == stmt
}

type pgFulltextIndexDB interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

const pgFulltextIndexSQL = `SELECT pg_catalog.pg_get_expr(x.indexprs, x.indrelid),
       COALESCE(x.indisvalid AND x.indisready AND NOT x.indisunique AND NOT x.indisprimary
       AND COALESCE((pg_catalog.row_to_json(x)->>'indislive')::boolean, true)
       AND COALESCE((pg_catalog.row_to_json(x)->>'indisusable')::boolean, true)
       AND x.indpred IS NULL AND x.indnatts=1 AND x.indkey[0]=0
       AND am.amname='gin' AND oc.opcname='tsvector_ops' AND ons.nspname='pg_catalog', false)
FROM pg_catalog.pg_index x
JOIN pg_catalog.pg_class t ON t.oid=x.indrelid
JOIN pg_catalog.pg_class i ON i.oid=x.indexrelid AND i.relnamespace=t.relnamespace
JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
JOIN pg_catalog.pg_am am ON am.oid=i.relam
LEFT JOIN pg_catalog.pg_opclass oc ON oc.oid=x.indclass[0] AND oc.opcmethod=am.oid
LEFT JOIN pg_catalog.pg_namespace ons ON ons.oid=oc.opcnamespace
WHERE n.nspname=$1 AND t.relname=$2 AND i.relname=$3`

func inspectPGFulltextIndex(ctx context.Context, db pgFulltextIndexDB, obj repair.Object) (repair.Verification, error) {
	idx := obj.Index
	if idx == nil || !validPGFulltextIndex(*idx) || obj.Schema == "" ||
		obj.Table == "" || obj.Table != idx.TableName || obj.TargetName == "" ||
		obj.TargetName != dialect.IndexName(dialect.NewPostgres("postgres"), *idx) {
		return verification("unknown", "缺少可核验的 PostgreSQL 全文索引计划")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return repair.Verification{}, err
	}
	defer tx.Rollback()
	// Deparse with a fixed path so a shadowing function, operator or text
	// search configuration is printed with its own schema.
	if _, err = tx.ExecContext(ctx, "SET LOCAL search_path TO pg_catalog, pg_temp"); err != nil {
		return repair.Verification{}, err
	}
	var expr sql.NullString
	var expectedProperties bool
	err = tx.QueryRowContext(ctx, pgFulltextIndexSQL, obj.Schema, obj.Table, obj.TargetName).Scan(&expr, &expectedProperties)
	if errors.Is(err, sql.ErrNoRows) {
		return verification("failed", "目标表不存在该名称的全文索引")
	}
	if err != nil {
		return repair.Verification{}, err
	}
	if !expectedProperties || !expr.Valid || !matchesPGFulltextExpression(expr.String, idx.Columns) {
		return verification("failed", "全文索引表达式、配置、GIN 操作符类或有效状态与原计划不符")
	}
	return verification("succeeded", "全文索引所属表、表达式、GIN 操作符类和有效状态核验通过")
}

// Match the PostgreSQL deparse of the generated expression. Parentheses and
// redundant text casts can vary, while the function, configuration, literals
// and ordered column references are fixed.
func matchesPGFulltextExpression(expression string, columns []string) bool {
	tokens, err := sqlsyntax.Lex(expression)
	if err != nil || len(columns) == 0 {
		return false
	}
	args := pgFulltextCallArgs(tokens, "to_tsvector")
	if len(args) != 2 || !(pgFulltextLiteral(args[0], "simple", "regconfig") ||
		pgFulltextLiteral(args[0], "pg_catalog.simple", "regconfig")) {
		return false
	}
	var parts [][]sqlsyntax.Token
	if !flattenPGFulltextConcat(args[1], &parts) || len(parts) != 2*len(columns)-1 {
		return false
	}
	for i, column := range columns {
		if i != 0 && !pgFulltextLiteral(parts[2*i-1], " ", "text") {
			return false
		}
		coalesce := pgFulltextCallArgs(parts[2*i], "coalesce")
		if len(coalesce) != 2 || !pgFulltextLiteral(coalesce[1], "", "text") {
			return false
		}
		key, _ := trimPGFulltextCast(coalesce[0], "text")
		if len(key) != 1 || !(key[0].Kind == 'i' && key[0].Text == column ||
			key[0].Kind == 'q' && strings.HasPrefix(key[0].Text, `"`) && key[0].Value == column) {
			return false
		}
	}
	return true
}

func pgFulltextCallArgs(tokens []sqlsyntax.Token, function string) [][]sqlsyntax.Token {
	tokens = trimPGFulltextParens(tokens)
	if function == "to_tsvector" && len(tokens) >= 2 && tokens[0].Is("pg_catalog") && tokens[1].Is(".") {
		tokens = tokens[2:]
	}
	if len(tokens) < 4 || !tokens[0].Is(function) || !tokens[1].Is("(") || !tokens[len(tokens)-1].Is(")") {
		return nil
	}
	return splitPGFulltextTokens(tokens[2:len(tokens)-1], ",")
}

func pgFulltextLiteral(tokens []sqlsyntax.Token, value, typ string) bool {
	tokens, cast := trimPGFulltextCast(tokens, typ)
	return cast && len(tokens) == 1 && tokens[0].Kind == 's' && tokens[0].Value == value
}

func trimPGFulltextCast(tokens []sqlsyntax.Token, typ string) ([]sqlsyntax.Token, bool) {
	tokens = trimPGFulltextParens(tokens)
	end := len(tokens) - 1
	if end < 1 || !tokens[end].Is(typ) {
		return tokens, false
	}
	if end >= 3 && tokens[end-1].Is(".") && tokens[end-2].Is("pg_catalog") {
		end -= 2
	}
	if tokens[end-1].Is("::") {
		return trimPGFulltextParens(tokens[:end-1]), true
	}
	return tokens, false
}

func flattenPGFulltextConcat(tokens []sqlsyntax.Token, parts *[][]sqlsyntax.Token) bool {
	tokens = trimPGFulltextParens(tokens)
	operands := splitPGFulltextTokens(tokens, "||")
	switch len(operands) {
	case 0:
		return false
	case 1:
		*parts = append(*parts, tokens)
	default:
		if !flattenPGFulltextConcat(operands[0], parts) {
			return false
		}
		// SQL parses consecutive concatenations left to right. A grouped
		// right operand would make the query and index expression trees differ.
		for _, operand := range operands[1:] {
			operand = trimPGFulltextParens(operand)
			if len(splitPGFulltextTokens(operand, "||")) != 1 {
				return false
			}
			*parts = append(*parts, operand)
		}
	}
	return true
}

func trimPGFulltextParens(tokens []sqlsyntax.Token) []sqlsyntax.Token {
	for len(tokens) >= 2 && tokens[0].Is("(") && tokens[len(tokens)-1].Is(")") {
		inner := tokens[1 : len(tokens)-1]
		if len(splitPGFulltextTokens(inner, "")) == 0 {
			break
		}
		tokens = inner
	}
	return tokens
}

func splitPGFulltextTokens(tokens []sqlsyntax.Token, separator string) [][]sqlsyntax.Token {
	var parts [][]sqlsyntax.Token
	depth, start := 0, 0
	for i, token := range tokens {
		switch {
		case token.Is("("):
			depth++
		case token.Is(")"):
			depth--
		case depth == 0 && token.Is(separator):
			if i == start {
				return nil
			}
			parts = append(parts, tokens[start:i])
			start = i + 1
		}
		if depth < 0 {
			return nil
		}
	}
	if depth != 0 || start == len(tokens) {
		return nil
	}
	return append(parts, tokens[start:])
}
