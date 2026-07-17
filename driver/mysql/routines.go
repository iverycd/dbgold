package mysql

import (
	"database/sql"
	"dbgold/schema"
	"fmt"
	"strings"
)

// ExtractRoutines 返回源库所有自定义函数和存储过程的原始 DDL。
// 不做任何语法转换，用 DELIMITER 包裹以便整体在 MySQL 客户端回放。
func (d *Driver) ExtractRoutines(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	rows, err := d.db.Query(
		`SELECT ROUTINE_NAME, ROUTINE_TYPE FROM INFORMATION_SCHEMA.ROUTINES
		 WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_TYPE, ROUTINE_NAME`, dbName)
	if err != nil {
		return nil, err
	}
	type meta struct{ name, typ string }
	var metas []meta
	for rows.Next() {
		var m meta
		if err := rows.Scan(&m.name, &m.typ); err != nil {
			rows.Close()
			return nil, err
		}
		metas = append(metas, m)
	}
	rows.Close()

	var routines []schema.Routine
	for _, m := range metas {
		ddl, err := d.showCreateRoutine(m.typ, dbName, m.name)
		if err != nil {
			return nil, fmt.Errorf("show create %s %s: %w", m.typ, m.name, err)
		}
		if ddl == "" {
			continue
		}
		body := wrapMySQLDDL(ddl)
		routines = append(routines, schema.Routine{Name: m.name, Type: m.typ, Body: body})
	}
	return routines, nil
}

// ExtractTriggers 返回源库所有触发器的原始 DDL。
// 不做任何语法转换，用 DELIMITER 包裹以便整体在 MySQL 客户端回放。
func (d *Driver) ExtractTriggers(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	rows, err := d.db.Query(
		`SELECT TRIGGER_NAME FROM INFORMATION_SCHEMA.TRIGGERS
		 WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME`, dbName)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var triggers []schema.Routine
	for _, name := range names {
		ddl, err := d.showCreateTrigger(dbName, name)
		if err != nil {
			return nil, fmt.Errorf("show create trigger %s: %w", name, err)
		}
		if ddl == "" {
			continue
		}
		body := wrapMySQLDDL(ddl)
		triggers = append(triggers, schema.Routine{Name: name, Type: "TRIGGER", Body: body})
	}
	return triggers, nil
}

// showCreateRoutine 执行 SHOW CREATE PROCEDURE/FUNCTION，返回 "Create ..." 列的内容。
// SHOW CREATE 的列数随 MySQL 版本变化，故动态定位 Create 列。
func (d *Driver) showCreateRoutine(routineType, dbName, name string) (string, error) {
	kind := "PROCEDURE"
	if strings.EqualFold(routineType, "FUNCTION") {
		kind = "FUNCTION"
	}
	q := fmt.Sprintf("SHOW CREATE %s `%s`.`%s`", kind, dbName, name)
	rows, err := d.db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	createIdx := findCreateColumn(cols)
	if createIdx < 0 {
		return "", fmt.Errorf("no create column in SHOW CREATE %s", kind)
	}
	if !rows.Next() {
		return "", rows.Err()
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]interface{}, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return "", err
	}
	return string(raw[createIdx]), nil
}

// showCreateTrigger 执行 SHOW CREATE TRIGGER，返回 "Create Trigger" 列的内容。
// SHOW CREATE 的列数随 MySQL 版本变化，故动态定位 Create 列。
func (d *Driver) showCreateTrigger(dbName, name string) (string, error) {
	q := fmt.Sprintf("SHOW CREATE TRIGGER `%s`.`%s`", escapeIdentifier(dbName), escapeIdentifier(name))
	rows, err := d.db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	createIdx := findCreateColumn(cols)
	if createIdx < 0 {
		return "", fmt.Errorf("no create column in SHOW CREATE TRIGGER")
	}
	if !rows.Next() {
		return "", rows.Err()
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]interface{}, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return "", err
	}
	return string(raw[createIdx]), nil
}

func findCreateColumn(columns []string) int {
	for i, column := range columns {
		normalized := strings.ToLower(strings.TrimSpace(column))
		// MySQL 8 的 SHOW CREATE TRIGGER 使用 SQL Original Statement，
		// 部分兼容实现使用 Create Trigger；例程则使用 Create Procedure/Function。
		if strings.HasPrefix(normalized, "create ") || normalized == "sql original statement" {
			return i
		}
	}
	return -1
}

func escapeIdentifier(identifier string) string {
	return strings.ReplaceAll(identifier, "`", "``")
}

func wrapMySQLDDL(ddl string) string {
	return "DELIMITER $$\n" + strings.TrimRight(ddl, "; \t\n\r") + "$$\nDELIMITER ;"
}
