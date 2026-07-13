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
		src := strings.TrimRight(bodies[k].String(), "; \t\n\r/")
		body := "CREATE OR REPLACE " + src + "\n/"
		routines = append(routines, schema.Routine{Name: k.name, Type: k.typ, Body: body})
	}
	return routines, nil
}
