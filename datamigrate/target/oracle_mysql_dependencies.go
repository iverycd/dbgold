package target

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/dialect"
)

// MySQLForeignKey is an observed target dependency, not a source migration
// object. RestoreSQL retains the original column order and referential actions.
type MySQLForeignKey struct {
	Schema, Table, ConstraintName string
	RefSchema, RefTable           string
	Columns, RefColumns           []string
	OnDelete, OnUpdate            string
	DropSQL, RestoreSQL           string
}

// OracleMySQLSchema resolves the database without changing pooled sessions or
// the writer. A configured schema takes precedence over the DSN's database.
func (w *MySQLWriter) OracleMySQLSchema(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w.schema != "" {
		return w.schema, nil
	}
	var schema sql.NullString
	if err := w.db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&schema); err != nil {
		return "", fmt.Errorf("读取目标 MySQL database: %w", err)
	}
	if !schema.Valid || schema.String == "" {
		return "", fmt.Errorf("必须指定目标 MySQL database")
	}
	return schema.String, nil
}

// OracleMySQLLowerCaseTableNames establishes how target identifiers must be
// compared when deciding whether both ends of an observed FK will be rebuilt.
func (w *MySQLWriter) OracleMySQLLowerCaseTableNames(ctx context.Context) (int, error) {
	var mode int
	if err := w.db.QueryRowContext(ctx, `SELECT @@lower_case_table_names`).Scan(&mode); err != nil {
		return 0, fmt.Errorf("读取目标 MySQL 表名大小写设置: %w", err)
	}
	if mode < 0 || mode > 2 {
		return 0, fmt.Errorf("无法识别目标 MySQL lower_case_table_names=%d", mode)
	}
	return mode, nil
}

var oracleMySQLVersionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// OracleMySQLVersion probes only the capability needed by the Oracle table
// planner. It deliberately does not apply PG-specific session or preflight
// requirements, nor limit current MySQL releases to the PG certification list.
func (w *MySQLWriter) OracleMySQLVersion(ctx context.Context) (dialect.MySQLVersion, error) {
	var raw string
	if err := w.db.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&raw); err != nil {
		return dialect.MySQLVersion{}, fmt.Errorf("读取目标 MySQL 版本: %w", err)
	}
	return parseOracleMySQLVersion(raw)
}

func parseOracleMySQLVersion(raw string) (dialect.MySQLVersion, error) {
	v := dialect.MySQLVersion{}
	if strings.Contains(strings.ToLower(raw), "mariadb") {
		return v, fmt.Errorf("Oracle → MySQL 宽表规划不支持 MariaDB 版本 %q", raw)
	}
	parts := oracleMySQLVersionRE.FindStringSubmatch(strings.TrimSpace(raw))
	if parts == nil {
		return v, fmt.Errorf("无法解析目标 MySQL 版本 %q", raw)
	}
	values := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, out := range values {
		n, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return dialect.MySQLVersion{}, fmt.Errorf("无法解析目标 MySQL 版本 %q: %w", raw, err)
		}
		*out = n
	}
	if !v.AtLeast(5, 7, 0) {
		return dialect.MySQLVersion{}, fmt.Errorf("Oracle → MySQL 宽表规划要求 MySQL 5.7+，当前 %s", raw)
	}
	return v, nil
}

const oracleMySQLForeignKeyQuery = `SELECT k.TABLE_SCHEMA,k.TABLE_NAME,k.CONSTRAINT_NAME,
 k.REFERENCED_TABLE_SCHEMA,k.REFERENCED_TABLE_NAME,k.COLUMN_NAME,k.REFERENCED_COLUMN_NAME,
 k.ORDINAL_POSITION,r.DELETE_RULE,r.UPDATE_RULE
 FROM information_schema.KEY_COLUMN_USAGE k
 LEFT JOIN information_schema.REFERENTIAL_CONSTRAINTS r
 ON r.CONSTRAINT_SCHEMA=k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME=k.CONSTRAINT_NAME AND r.TABLE_NAME=k.TABLE_NAME
 WHERE k.REFERENCED_TABLE_NAME IS NOT NULL
 AND (k.TABLE_SCHEMA=? OR k.REFERENCED_TABLE_SCHEMA=?)
 ORDER BY k.TABLE_SCHEMA,k.TABLE_NAME,k.CONSTRAINT_NAME,k.ORDINAL_POSITION`

// OracleMySQLForeignKeys includes incoming references from other databases and
// outgoing references from this database. No table-name filter may hide a
// retained child whose foreign key must protect a selected parent from DROP.
func (w *MySQLWriter) OracleMySQLForeignKeys(ctx context.Context) ([]MySQLForeignKey, error) {
	schema, err := w.OracleMySQLSchema(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := w.db.QueryContext(ctx, oracleMySQLForeignKeyQuery, schema, schema)
	if err != nil {
		return nil, fmt.Errorf("读取目标 MySQL 外键依赖: %w", err)
	}
	defer rows.Close()
	type identity struct{ schema, table, name string }
	positions := map[identity]int{}
	result := []MySQLForeignKey{}
	for rows.Next() {
		var fk MySQLForeignKey
		var column, refColumn string
		var ordinal int
		if err = rows.Scan(&fk.Schema, &fk.Table, &fk.ConstraintName, &fk.RefSchema, &fk.RefTable,
			&column, &refColumn, &ordinal, &fk.OnDelete, &fk.OnUpdate); err != nil {
			return nil, fmt.Errorf("读取目标 MySQL 外键列: %w", err)
		}
		if fk.Schema == "" || fk.Table == "" || fk.ConstraintName == "" || fk.RefSchema == "" || fk.RefTable == "" || column == "" || refColumn == "" {
			return nil, fmt.Errorf("目标 MySQL 外键元数据不完整，不能安全覆盖表")
		}
		fk.OnDelete, err = oracleMySQLFKAction(fk.OnDelete)
		if err != nil {
			return nil, fmt.Errorf("外键 %s.%s.%s: %w", fk.Schema, fk.Table, fk.ConstraintName, err)
		}
		fk.OnUpdate, err = oracleMySQLFKAction(fk.OnUpdate)
		if err != nil {
			return nil, fmt.Errorf("外键 %s.%s.%s: %w", fk.Schema, fk.Table, fk.ConstraintName, err)
		}
		key := identity{fk.Schema, fk.Table, fk.ConstraintName}
		idx, exists := positions[key]
		if !exists {
			idx = len(result)
			positions[key] = idx
			result = append(result, fk)
		}
		current := &result[idx]
		if ordinal != len(current.Columns)+1 || current.RefSchema != fk.RefSchema || current.RefTable != fk.RefTable || current.OnDelete != fk.OnDelete || current.OnUpdate != fk.OnUpdate {
			return nil, fmt.Errorf("目标外键 %s.%s.%s 的列顺序或定义不完整，不能安全覆盖表", fk.Schema, fk.Table, fk.ConstraintName)
		}
		current.Columns = append(current.Columns, column)
		current.RefColumns = append(current.RefColumns, refColumn)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("读取目标 MySQL 外键依赖: %w", err)
	}
	for i := range result {
		fk := &result[i]
		quoted := make([]string, len(fk.Columns))
		refQuoted := make([]string, len(fk.RefColumns))
		for j := range fk.Columns {
			quoted[j], refQuoted[j] = w.quoteIdent(fk.Columns[j]), w.quoteIdent(fk.RefColumns[j])
		}
		child := w.quoteIdent(fk.Schema) + "." + w.quoteIdent(fk.Table)
		parent := w.quoteIdent(fk.RefSchema) + "." + w.quoteIdent(fk.RefTable)
		fk.DropSQL = "ALTER TABLE " + child + " DROP FOREIGN KEY " + w.quoteIdent(fk.ConstraintName)
		fk.RestoreSQL = "ALTER TABLE " + child + " ADD CONSTRAINT " + w.quoteIdent(fk.ConstraintName) +
			" FOREIGN KEY (" + strings.Join(quoted, ", ") + ") REFERENCES " + parent +
			" (" + strings.Join(refQuoted, ", ") + ") ON DELETE " + fk.OnDelete + " ON UPDATE " + fk.OnUpdate
	}
	return result, nil
}

func oracleMySQLFKAction(raw string) (string, error) {
	action := strings.ToUpper(strings.Join(strings.Fields(raw), " "))
	switch action {
	case "RESTRICT", "CASCADE", "SET NULL", "NO ACTION":
		return action, nil
	default:
		return "", fmt.Errorf("不支持或缺失的 MySQL 外键动作 %q", raw)
	}
}

// ExecuteOracleMySQLDependency keeps each DROP/restore inside the existing
// durable DDL observer. Foreign-key checking stays enabled for every statement.
func (w *MySQLWriter) ExecuteOracleMySQLDependency(ctx context.Context, query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("目标外键依赖 SQL 不能为空")
	}
	_, err := execObserved(ctx, w.db, query)
	if err != nil {
		return fmt.Errorf("执行目标外键依赖 SQL [%s]: %w", query, err)
	}
	return nil
}
