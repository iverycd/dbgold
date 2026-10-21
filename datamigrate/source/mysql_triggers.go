package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GetTriggers reads original spelling and source SQL. Conversion belongs to the
// migration planner, never to the source metadata reader.
func (r *MySQLReader) GetTriggers(ctx context.Context) ([]TriggerInfo, error) {
	rows, err := r.queryContext(ctx, `SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, TRIGGER_SCHEMA,
		ACTION_TIMING, EVENT_MANIPULATION, ACTION_STATEMENT, SQL_MODE, DEFINER, ACTION_ORDER
		FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?
		ORDER BY EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION, ACTION_ORDER, TRIGGER_NAME`, r.dbName)
	if err != nil {
		return nil, fmt.Errorf("读取 MySQL 触发器: %w", err)
	}
	var out []TriggerInfo
	for rows.Next() {
		var t TriggerInfo
		var body sql.NullString
		if err = rows.Scan(&t.Name, &t.TableName, &t.Schema, &t.Timing, &t.Event, &body, &t.SQLMode, &t.Definer, &t.ActionOrder); err != nil {
			rows.Close()
			return nil, err
		}
		t.Body = body.String
		if !body.Valid || strings.TrimSpace(body.String) == "" {
			t.MetadataError = "源库未返回触发器正文，请检查 TRIGGER 权限"
		}
		out = append(out, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		ddl, e := r.readTriggerDDL(ctx, out[i].Schema, out[i].Name)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			out[i].MetadataError = fmt.Sprintf("读取触发器原始 DDL: %v", e)
		} else {
			out[i].SourceDDL = ddl
		}
	}
	return out, nil
}

func (r *MySQLReader) readTriggerDDL(ctx context.Context, schema, name string) (string, error) {
	quote := func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
	rows, err := r.queryContext(ctx, "SHOW CREATE TRIGGER "+quote(schema)+"."+quote(name))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	idx := -1
	for i, col := range cols {
		if strings.EqualFold(col, "SQL Original Statement") || strings.HasPrefix(strings.ToLower(col), "create ") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("SHOW CREATE TRIGGER 缺少原始语句列")
	}
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return "", err
		}
		return "", sql.ErrNoRows
	}
	values := make([]sql.NullString, len(cols))
	args := make([]any, len(cols))
	for i := range args {
		args[i] = &values[i]
	}
	if err = rows.Scan(args...); err != nil {
		return "", err
	}
	if !values[idx].Valid || strings.TrimSpace(values[idx].String) == "" {
		return "", fmt.Errorf("源库未返回触发器 DDL，请检查 TRIGGER 权限")
	}
	ddl := values[idx].String
	if rows.Next() {
		return "", fmt.Errorf("SHOW CREATE TRIGGER 返回多条记录")
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	return ddl, nil
}
