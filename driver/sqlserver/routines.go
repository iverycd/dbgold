package sqlserver

import (
	"database/sql"
	"dbgold/schema"
	"fmt"
	"strings"
)

// ExtractRoutines 返回 SQL Server 源库的自定义函数和存储过程原始 T-SQL 定义。
// 从 sys.sql_modules 取 definition（含完整 CREATE 语句），不做任何跨库语法转换。
func (d *Driver) ExtractRoutines(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	rows, err := d.db.Query(`
		SELECT o.name,
		       CASE WHEN o.type = 'P' THEN 'PROCEDURE' ELSE 'FUNCTION' END AS routine_type,
		       m.definition
		FROM sys.sql_modules m
		JOIN sys.objects o ON m.object_id = o.object_id
		WHERE o.type IN ('P','FN','IF','TF')
		  AND o.is_ms_shipped = 0
		ORDER BY o.type, o.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routines []schema.Routine
	for rows.Next() {
		var name, typ, def string
		if err := rows.Scan(&name, &typ, &def); err != nil {
			return nil, err
		}
		body := strings.TrimRight(def, "; \t\n\r") + "\nGO"
		routines = append(routines, schema.Routine{Name: name, Type: typ, Body: body})
	}
	return routines, rows.Err()
}

// ExtractTriggers 返回 SQL Server 源库触发器的原始 T-SQL 定义。
// 不连接 sys.trigger_events，避免多事件触发器因事件行展开而被重复导出。
func (d *Driver) ExtractTriggers(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	rows, err := d.db.Query(`
		SELECT t.object_id, t.name, m.definition
		FROM sys.triggers t
		JOIN sys.sql_modules m ON m.object_id = t.object_id
		ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []schema.Routine
	seen := make(map[int64]struct{})
	for rows.Next() {
		var objectID int64
		var name string
		var definition sql.NullString
		if err := rows.Scan(&objectID, &name, &definition); err != nil {
			return nil, err
		}
		appendUniqueTrigger(&triggers, seen, objectID, name, definition.String, definition.Valid)
	}
	return triggers, rows.Err()
}

func appendUniqueTrigger(triggers *[]schema.Routine, seen map[int64]struct{}, objectID int64, name, definition string, valid bool) {
	if _, ok := seen[objectID]; ok || !valid || strings.TrimSpace(definition) == "" {
		return
	}
	seen[objectID] = struct{}{}
	body := strings.TrimRight(definition, "; \t\n\r") + "\nGO"
	*triggers = append(*triggers, schema.Routine{Name: name, Type: "TRIGGER", Body: body})
}
