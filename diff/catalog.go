package diff

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// ReadCatalog inventories the full namespace and reads definitions for the
// requested exact table names. A nil selection reads all table definitions.
// It only accesses system catalogs and never reads application rows.
func ReadCatalog(ctx context.Context, q Queryer, product, namespace string, tables []string) (Catalog, error) {
	catalog := Catalog{
		Product: product, Namespace: namespace,
		Tables: make(map[string]*Table), Occupied: make(map[string]string),
	}
	selected := make(map[string]bool, len(tables))
	for _, table := range tables {
		selected[table] = true
	}
	if tables == nil {
		selected = nil
	}
	var err error
	switch {
	case product == "mysql":
		err = readMySQLCatalog(ctx, q, &catalog, selected)
	case product == "dameng":
		err = readDamengCatalog(ctx, q, &catalog, selected)
	case source.IsPGFamily(product):
		err = readPGCatalog(ctx, q, &catalog, selected)
	default:
		err = fmt.Errorf("数据库类型暂不支持结构比对: %s", product)
	}
	if err != nil {
		return Catalog{}, fmt.Errorf("读取 %s 的 %s 结构: %w", product, namespace, err)
	}
	for _, table := range catalog.Tables {
		if len(table.Columns) == 0 {
			table.Unsupported = catalogReason(table.Unsupported, "表的列定义不可见或读取时已发生变化")
		}
	}
	return catalog, nil
}

func catalogSelected(selected map[string]bool, table string) bool {
	return selected == nil || selected[table]
}

func catalogRows(ctx context.Context, q Queryer, stage, query string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", stage, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("%s: %w", stage, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w", stage, err)
	}
	return nil
}

func catalogReason(current, next string) string {
	if current == "" {
		return next
	}
	if next == "" || strings.Contains(current, next) {
		return current
	}
	return current + "；" + next
}

func catalogAppendKey(table *Table, key Key) {
	if key.Kind == "index" {
		table.Indexes = append(table.Indexes, key)
		return
	}
	table.Keys = append(table.Keys, key)
}
