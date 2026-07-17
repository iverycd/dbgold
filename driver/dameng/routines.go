package dameng

import (
	"dbgold/schema"
	"fmt"
	"strings"
)

// ExtractRoutines 返回达梦源库的函数、存储过程及包的原始源码。
// 达梦兼容 Oracle 数据字典，从 ALL_SOURCE 按行拼接，不做任何跨库语法转换。
func (d *Driver) ExtractRoutines(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	// 达梦 OWNER 大小写与连接配置的 Database 字段一致，直接透传（与 datamigrate reader 保持一致）
	rows, err := d.db.Query(
		`SELECT NAME, TYPE, TEXT FROM ALL_SOURCE
		 WHERE OWNER = ? AND TYPE IN ('PROCEDURE','FUNCTION','PACKAGE','PACKAGE BODY')
		 ORDER BY TYPE, NAME, LINE`, dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ name, typ string }
	var order []key
	bodies := map[key]*strings.Builder{}
	for rows.Next() {
		var name, typ, text string
		if err := rows.Scan(&name, &typ, &text); err != nil {
			return nil, err
		}
		k := key{name, typ}
		b, ok := bodies[k]
		if !ok {
			b = &strings.Builder{}
			bodies[k] = b
			order = append(order, k)
		}
		b.WriteString(text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var routines []schema.Routine
	for _, k := range order {
		body := wrapPLSQLSource(bodies[k].String())
		routines = append(routines, schema.Routine{Name: k.name, Type: k.typ, Body: body})
	}
	return routines, nil
}

// ExtractTriggers 返回达梦源库触发器的原始源码。
// OWNER 透传连接配置中的 Database 字段，占位符使用达梦驱动支持的 ?。
func (d *Driver) ExtractTriggers(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	rows, err := d.db.Query(
		`SELECT NAME, TEXT FROM ALL_SOURCE
		 WHERE OWNER = ? AND TYPE = 'TRIGGER'
		 ORDER BY NAME, LINE`, dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var order []string
	bodies := map[string]*strings.Builder{}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			return nil, err
		}
		b, ok := bodies[name]
		if !ok {
			b = &strings.Builder{}
			bodies[name] = b
			order = append(order, name)
		}
		b.WriteString(text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var triggers []schema.Routine
	for _, name := range order {
		body := wrapPLSQLSource(bodies[name].String())
		triggers = append(triggers, schema.Routine{Name: name, Type: "TRIGGER", Body: body})
	}
	return triggers, nil
}

func wrapPLSQLSource(source string) string {
	// 保留 PL/SQL 块末尾的分号，只移除已有的客户端分隔符和空白。
	source = strings.TrimRight(source, " \t\n\r/")
	return "CREATE OR REPLACE " + source + "\n/"
}
