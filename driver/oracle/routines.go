package oracle

import (
	"dbgold/schema"
	"fmt"
	"strings"
)

// ExtractRoutines 返回 Oracle 源库的函数、存储过程及包的原始 PL/SQL 源码。
// 从 ALL_SOURCE 按行拼接，不做任何跨库语法转换。
func (d *Driver) ExtractRoutines(dbName string) ([]schema.Routine, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	owner := strings.ToUpper(dbName)
	rows, err := d.db.Query(
		`SELECT NAME, TYPE, TEXT FROM ALL_SOURCE
		 WHERE OWNER = :1 AND TYPE IN ('PROCEDURE','FUNCTION','PACKAGE','PACKAGE BODY')
		 ORDER BY TYPE, NAME, LINE`, owner)
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
		// ALL_SOURCE 首行以 "PROCEDURE name..." 开头，补上 CREATE OR REPLACE。
		body := "CREATE OR REPLACE " + src + "\n/"
		routines = append(routines, schema.Routine{Name: k.name, Type: k.typ, Body: body})
	}
	return routines, nil
}
