package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/sqlsyntax"
)

// CheckConstraintInfo retains source rules that need manual adaptation.
type CheckConstraintInfo struct {
	TableName   string
	Name        string
	Definition  string
	Unsupported string
}

type oracleSQLServerUniqueConstraint struct {
	CheckConstraintInfo
	indexName, indexOwner string
}

func (r *OracleSQLServerReader) unsafeUniqueConstraints(ctx context.Context) ([]oracleSQLServerUniqueConstraint, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT TABLE_NAME,CONSTRAINT_NAME,INDEX_NAME,INDEX_OWNER,STATUS,VALIDATED,DEFERRABLE,DEFERRED
 FROM ALL_CONSTRAINTS WHERE OWNER=:1 AND CONSTRAINT_TYPE='U'
 AND (STATUS<>'ENABLED' OR VALIDATED<>'VALIDATED' OR DEFERRABLE<>'NOT DEFERRABLE' OR DEFERRED<>'IMMEDIATE')
 ORDER BY TABLE_NAME,CONSTRAINT_NAME`, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []oracleSQLServerUniqueConstraint
	for rows.Next() {
		var rule oracleSQLServerUniqueConstraint
		var index, indexOwner sql.NullString
		var status, validated, deferrable, deferred string
		if err := rows.Scan(&rule.TableName, &rule.Name, &index, &indexOwner, &status, &validated, &deferrable, &deferred); err != nil {
			return nil, err
		}
		rule.indexName, rule.indexOwner = index.String, indexOwner.String
		rule.Unsupported = fmt.Sprintf("禁用、未验证或可延迟 UNIQUE 约束需要手工适配（%s, %s, %s, %s）", status, validated, deferrable, deferred)
		result = append(result, rule)
	}
	return result, rows.Err()
}

func (r *OracleSQLServerReader) GetIndexes(ctx context.Context) ([]IndexInfo, error) {
	indexes, err := r.OracleDamengReader.GetIndexes(ctx)
	if err != nil || len(indexes) == 0 {
		return indexes, err
	}
	rules, err := r.unsafeUniqueConstraints(ctx)
	if err != nil {
		return nil, err
	}
	unsafe := make(map[[2]string]string)
	for _, rule := range rules {
		if rule.indexOwner == r.owner && rule.indexName != "" {
			unsafe[[2]string{rule.TableName, rule.indexName}] = rule.Unsupported
		}
	}
	for i := range indexes {
		index := &indexes[i]
		if reason := unsafe[[2]string{index.TableName, index.IndexName}]; index.IsUnique && reason != "" && index.Unsupported == "" {
			index.Unsupported = reason
		}
	}
	return indexes, nil
}

func (r *OracleSQLServerReader) SQLServerUnsupportedConstraints(ctx context.Context) ([]CheckConstraintInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT TABLE_NAME,CONSTRAINT_NAME,SEARCH_CONDITION FROM ALL_CONSTRAINTS
 WHERE OWNER=:1 AND CONSTRAINT_TYPE='C' ORDER BY TABLE_NAME,CONSTRAINT_NAME`, r.owner)
	if err != nil {
		return nil, err
	}
	var all []CheckConstraintInfo
	for rows.Next() {
		var check CheckConstraintInfo
		var condition sql.NullString
		if err := rows.Scan(&check.TableName, &check.Name, &condition); err != nil {
			rows.Close()
			return nil, err
		}
		check.Definition = condition.String
		check.Unsupported = "Oracle CHECK 约束需要手工适配"
		if strings.TrimSpace(check.Definition) == "" {
			check.Unsupported = "Oracle CHECK 约束表达式不可读取"
		}
		all = append(all, check)
	}
	if err := pgFinishOptionalRows(rows); err != nil {
		return nil, err
	}
	var out []CheckConstraintInfo
	for _, check := range all {
		if column := oracleNotNullCheckColumn(check.Definition); column != "" {
			info, err := r.GetTableDDLInfo(ctx, check.TableName)
			if failure.IsTaskError(err) {
				return nil, err
			}
			if err == nil && oracleColumnNotNullable(info, column) {
				continue // The target column's NOT NULL already enforces this rule.
			}
		}
		out = append(out, check)
	}
	unique, err := r.unsafeUniqueConstraints(ctx)
	if err != nil {
		return nil, err
	}
	for _, rule := range unique {
		out = append(out, rule.CheckConstraintInfo)
	}
	return out, nil
}

func oracleNotNullCheckColumn(condition string) string {
	tokens, err := sqlsyntax.Lex(condition)
	if err != nil {
		return ""
	}
	for len(tokens) > 4 && tokens[0].Is("(") && tokens[len(tokens)-1].Is(")") {
		tokens = tokens[1 : len(tokens)-1]
	}
	if len(tokens) == 4 && (tokens[0].Kind == 'q' || tokens[0].Kind == 'i') && tokens[1].Is("IS") && tokens[2].Is("NOT") && tokens[3].Is("NULL") {
		return oracleDMIdentifier(tokens[0].Text)
	}
	return ""
}

func oracleColumnNotNullable(info *TableDDLInfo, column string) bool {
	for _, c := range info.Columns {
		if c.Name == column {
			return !c.IsNullable
		}
	}
	return false
}

func (r *PostgresSQLServerReader) SQLServerUnsupportedConstraints(ctx context.Context) ([]CheckConstraintInfo, error) {
	definition := "NULL::text"
	if r.pgMetadataFunction("pg_get_constraintdef") {
		definition = "pg_get_constraintdef(c.oid)"
	}
	query := `SELECT t.relname,c.conname,` + definition + `,c.contype FROM ` + r.qcat("pg_constraint") + ` c JOIN ` + r.qcat("pg_class") + ` t ON t.oid=c.conrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND c.contype IN ('c','x') ORDER BY t.relname,c.conname`
	rows, err := r.db.QueryContext(ctx, query, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CheckConstraintInfo
	for rows.Next() {
		var check CheckConstraintInfo
		var definition sql.NullString
		var kind string
		if err := rows.Scan(&check.TableName, &check.Name, &definition, &kind); err != nil {
			return nil, err
		}
		check.Definition = definition.String
		check.Unsupported = "PG CHECK 约束需要手工适配"
		if kind == "x" {
			check.Unsupported = "PG 排除约束需要手工适配"
		}
		if strings.TrimSpace(check.Definition) == "" {
			check.Unsupported += "，源定义不可读取"
		}
		result = append(result, check)
	}
	return result, rows.Err()
}
