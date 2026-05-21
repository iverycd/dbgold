package mysql

import (
	"dbgold/schema"
	"fmt"
)

func (d *Driver) ExtractSchema(dbName string) (*schema.Schema, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	s := &schema.Schema{Name: dbName}

	tables, err := d.listTables(dbName)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	for _, tableName := range tables {
		t, err := d.extractTable(dbName, tableName)
		if err != nil {
			return nil, fmt.Errorf("extract table %s: %w", tableName, err)
		}
		s.Tables = append(s.Tables, *t)
	}
	return s, nil
}

func (d *Driver) ExtractFullObjects(dbName string) (*schema.FullSchema, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	base, err := d.ExtractSchema(dbName)
	if err != nil {
		return nil, err
	}
	full := &schema.FullSchema{Schema: *base}

	views, err := d.extractViews(dbName)
	if err != nil {
		return nil, fmt.Errorf("extract views: %w", err)
	}
	full.Views = views

	triggers, err := d.extractTriggers(dbName)
	if err != nil {
		return nil, fmt.Errorf("extract triggers: %w", err)
	}
	full.Triggers = triggers

	return full, nil
}

func (d *Driver) listTables(dbName string) ([]string, error) {
	rows, err := d.db.Query(
		"SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME",
		dbName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (d *Driver) extractTable(dbName, tableName string) (*schema.Table, error) {
	t := &schema.Table{Name: tableName}
	var err error

	t.Columns, err = d.extractColumns(dbName, tableName)
	if err != nil {
		return nil, err
	}
	t.Indexes, err = d.extractIndexes(dbName, tableName)
	if err != nil {
		return nil, err
	}
	t.ForeignKeys, err = d.extractForeignKeys(dbName, tableName)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (d *Driver) extractColumns(dbName, tableName string) ([]schema.Column, error) {
	rows, err := d.db.Query(`
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT,
		       COLUMN_KEY, EXTRA
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`,
		dbName, tableName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []schema.Column
	for rows.Next() {
		var name, colType, isNullable, colKey, extra string
		var defaultVal *string
		if err := rows.Scan(&name, &colType, &isNullable, &defaultVal, &colKey, &extra); err != nil {
			return nil, err
		}
		cols = append(cols, schema.Column{
			Name:          name,
			Type:          colType,
			Nullable:      isNullable == "YES",
			Default:       defaultVal,
			PrimaryKey:    colKey == "PRI",
			AutoIncrement: extra == "auto_increment",
		})
	}
	return cols, rows.Err()
}

func (d *Driver) extractIndexes(dbName, tableName string) ([]schema.Index, error) {
	rows, err := d.db.Query(`
		SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE
		FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME != 'PRIMARY'
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`,
		dbName, tableName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	idxMap := map[string]*schema.Index{}
	var order []string
	for rows.Next() {
		var idxName, colName string
		var nonUnique int
		if err := rows.Scan(&idxName, &colName, &nonUnique); err != nil {
			return nil, err
		}
		if _, exists := idxMap[idxName]; !exists {
			idxMap[idxName] = &schema.Index{Name: idxName, Unique: nonUnique == 0}
			order = append(order, idxName)
		}
		idxMap[idxName].Columns = append(idxMap[idxName].Columns, colName)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var indexes []schema.Index
	for _, name := range order {
		indexes = append(indexes, *idxMap[name])
	}
	return indexes, nil
}

func (d *Driver) extractForeignKeys(dbName, tableName string) ([]schema.ForeignKey, error) {
	rows, err := d.db.Query(`
		SELECT kcu.CONSTRAINT_NAME, kcu.COLUMN_NAME,
		       kcu.REFERENCED_TABLE_NAME, kcu.REFERENCED_COLUMN_NAME,
		       rc.DELETE_RULE, rc.UPDATE_RULE
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
		JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS rc
		  ON rc.CONSTRAINT_NAME = kcu.CONSTRAINT_NAME AND rc.CONSTRAINT_SCHEMA = kcu.TABLE_SCHEMA
		WHERE kcu.TABLE_SCHEMA = ? AND kcu.TABLE_NAME = ?
		  AND kcu.REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY kcu.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`,
		dbName, tableName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fkMap := map[string]*schema.ForeignKey{}
	var order []string
	for rows.Next() {
		var name, col, refTable, refCol, delRule, updRule string
		if err := rows.Scan(&name, &col, &refTable, &refCol, &delRule, &updRule); err != nil {
			return nil, err
		}
		if _, exists := fkMap[name]; !exists {
			fkMap[name] = &schema.ForeignKey{
				Name: name, RefTable: refTable,
				OnDelete: delRule, OnUpdate: updRule,
			}
			order = append(order, name)
		}
		fkMap[name].Columns = append(fkMap[name].Columns, col)
		fkMap[name].RefColumns = append(fkMap[name].RefColumns, refCol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var fks []schema.ForeignKey
	for _, name := range order {
		fks = append(fks, *fkMap[name])
	}
	return fks, nil
}

func (d *Driver) extractViews(dbName string) ([]schema.View, error) {
	rows, err := d.db.Query(
		"SELECT TABLE_NAME, VIEW_DEFINITION FROM INFORMATION_SCHEMA.VIEWS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME",
		dbName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []schema.View
	for rows.Next() {
		var v schema.View
		if err := rows.Scan(&v.Name, &v.Def); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}

func (d *Driver) extractTriggers(dbName string) ([]schema.Trigger, error) {
	rows, err := d.db.Query(`
		SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, EVENT_MANIPULATION,
		       ACTION_TIMING, ACTION_STATEMENT
		FROM INFORMATION_SCHEMA.TRIGGERS
		WHERE TRIGGER_SCHEMA = ?
		ORDER BY TRIGGER_NAME`,
		dbName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []schema.Trigger
	for rows.Next() {
		var tr schema.Trigger
		if err := rows.Scan(&tr.Name, &tr.Table, &tr.Event, &tr.Timing, &tr.Body); err != nil {
			return nil, err
		}
		triggers = append(triggers, tr)
	}
	return triggers, rows.Err()
}
