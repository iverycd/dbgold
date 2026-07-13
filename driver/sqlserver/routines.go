package sqlserver

import (
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
