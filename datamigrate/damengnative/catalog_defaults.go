package damengnative

import (
	"database/sql"
	"fmt"
)

// NativeDefaultOnNullColumns reads only native column attributes. It never
// evaluates a default expression or a routine embedded in a string.
func NativeDefaultOnNullColumns(ddl string) (map[string]bool, error) {
	return materializedDefaultOnNullColumns(ddl)
}

func (r *catalogReader) freezeNativeCopyDefaults(object Object, table *Table, sourceName string) error {
	wanted := map[string]bool{}
	for _, unit := range object.Units {
		if unit.Phase != "before_data" {
			continue
		}
		columns, err := NativeDefaultOnNullColumns(unit.SQL)
		if err != nil {
			// Other foundation units belong to indexes and constraints. The
			// source CREATE TABLE is independently validated during extraction.
			continue
		}
		for name := range columns {
			wanted[name] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(r.ctx, `SELECT COLUMN_NAME,DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`, r.schema, sourceName)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var expression sql.NullString
		if err = rows.Scan(&name, &expression); err != nil {
			return err
		}
		if !wanted[name] {
			continue
		}
		if !expression.Valid || expression.String == "" {
			return fmt.Errorf("列 %s 的 DEFAULT ON NULL 原生表达式不可读取", name)
		}
		for i := range table.Columns {
			if table.Columns[i].Name == name {
				table.Columns[i].DefaultOnNull = true
				table.Columns[i].DefaultSQL = expression.String
				delete(wanted, name)
				break
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(wanted) != 0 {
		return fmt.Errorf("表 %s 的 DEFAULT ON NULL 列清单不完整", table.Name)
	}
	return nil
}
