package sqlserver

import (
	"dbgold/schema"
	"fmt"
)

func (d *Driver) ExtractSchema(dbName string) (*schema.Schema, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	s := &schema.Schema{Name: dbName}

	tables, err := d.listTables()
	if err != nil {
		return nil, err
	}
	for _, tableName := range tables {
		t, err := d.extractTable(tableName)
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
	full.Views, _ = d.extractViews()
	full.Triggers, _ = d.extractTriggers()
	return full, nil
}

func (d *Driver) listTables() ([]string, error) {
	rows, err := d.db.Query(`
		SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_TYPE = 'BASE TABLE' AND TABLE_SCHEMA = 'dbo'
		ORDER BY TABLE_NAME`)
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

func (d *Driver) extractTable(tableName string) (*schema.Table, error) {
	t := &schema.Table{Name: tableName}
	var err error
	t.Columns, err = d.extractColumns(tableName)
	if err != nil {
		return nil, err
	}
	t.Indexes, err = d.extractIndexes(tableName)
	if err != nil {
		return nil, err
	}
	t.ForeignKeys, err = d.extractForeignKeys(tableName)
	if err != nil {
		return nil, err
	}
	t.Constraints, err = d.extractConstraints(tableName)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (d *Driver) extractColumns(tableName string) ([]schema.Column, error) {
	rows, err := d.db.Query(`
		SELECT c.COLUMN_NAME,
		       c.DATA_TYPE + CASE
		           WHEN c.CHARACTER_MAXIMUM_LENGTH IS NOT NULL
		           THEN '(' + CAST(c.CHARACTER_MAXIMUM_LENGTH AS VARCHAR) + ')'
		           ELSE '' END,
		       c.IS_NULLABLE,
		       c.COLUMN_DEFAULT,
		       CASE WHEN pk.COLUMN_NAME IS NOT NULL THEN 1 ELSE 0 END,
		       COLUMNPROPERTY(OBJECT_ID(c.TABLE_NAME), c.COLUMN_NAME, 'IsIdentity')
		FROM INFORMATION_SCHEMA.COLUMNS c
		LEFT JOIN (
		    SELECT ku.COLUMN_NAME FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
		    JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE ku
		      ON tc.CONSTRAINT_NAME = ku.CONSTRAINT_NAME
		    WHERE tc.CONSTRAINT_TYPE = 'PRIMARY KEY' AND tc.TABLE_NAME = @p1
		) pk ON pk.COLUMN_NAME = c.COLUMN_NAME
		WHERE c.TABLE_NAME = @p1 AND c.TABLE_SCHEMA = 'dbo'
		ORDER BY c.ORDINAL_POSITION`, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []schema.Column
	for rows.Next() {
		var c schema.Column
		var isNullable string
		var isPK, isIdentity int
		var defaultVal *string
		if err := rows.Scan(&c.Name, &c.Type, &isNullable, &defaultVal, &isPK, &isIdentity); err != nil {
			return nil, err
		}
		c.Nullable = isNullable == "YES"
		c.Default = defaultVal
		c.PrimaryKey = isPK == 1
		c.AutoIncrement = isIdentity == 1
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func (d *Driver) extractIndexes(tableName string) ([]schema.Index, error) {
	rows, err := d.db.Query(`
		SELECT i.name, c.name, i.is_unique
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = i.object_id AND c.column_id = ic.column_id
		WHERE OBJECT_NAME(i.object_id) = @p1
		  AND i.is_primary_key = 0 AND i.name IS NOT NULL
		ORDER BY i.name, ic.key_ordinal`, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idxMap := map[string]*schema.Index{}
	var order []string
	for rows.Next() {
		var idxName, colName string
		var unique bool
		if err := rows.Scan(&idxName, &colName, &unique); err != nil {
			return nil, err
		}
		if _, exists := idxMap[idxName]; !exists {
			idxMap[idxName] = &schema.Index{Name: idxName, Unique: unique}
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

func (d *Driver) extractForeignKeys(tableName string) ([]schema.ForeignKey, error) {
	rows, err := d.db.Query(`
		SELECT fk.name, c.name, rt.name, rc.name,
		       fk.delete_referential_action_desc, fk.update_referential_action_desc
		FROM sys.foreign_keys fk
		JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		JOIN sys.columns c ON c.object_id = fkc.parent_object_id AND c.column_id = fkc.parent_column_id
		JOIN sys.tables rt ON rt.object_id = fkc.referenced_object_id
		JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		WHERE OBJECT_NAME(fk.parent_object_id) = @p1
		ORDER BY fk.name, fkc.constraint_column_id`, tableName)
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
			fkMap[name] = &schema.ForeignKey{Name: name, RefTable: refTable, OnDelete: delRule, OnUpdate: updRule}
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

func (d *Driver) extractConstraints(tableName string) ([]schema.Constraint, error) {
	rows, err := d.db.Query(`
		SELECT tc.CONSTRAINT_NAME, tc.CONSTRAINT_TYPE,
		       cc.CHECK_CLAUSE
		FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
		LEFT JOIN INFORMATION_SCHEMA.CHECK_CONSTRAINTS cc
		  ON cc.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		WHERE tc.TABLE_NAME = @p1 AND tc.TABLE_SCHEMA = 'dbo'
		  AND tc.CONSTRAINT_TYPE IN ('CHECK', 'UNIQUE')
		ORDER BY tc.CONSTRAINT_NAME`, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var constraints []schema.Constraint
	for rows.Next() {
		var c schema.Constraint
		var def *string
		if err := rows.Scan(&c.Name, &c.Type, &def); err != nil {
			return nil, err
		}
		if def != nil {
			c.Def = *def
		}
		constraints = append(constraints, c)
	}
	return constraints, rows.Err()
}

func (d *Driver) extractViews() ([]schema.View, error) {
	rows, err := d.db.Query(`
		SELECT TABLE_NAME, VIEW_DEFINITION
		FROM INFORMATION_SCHEMA.VIEWS
		WHERE TABLE_SCHEMA = 'dbo'
		ORDER BY TABLE_NAME`)
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

func (d *Driver) extractTriggers() ([]schema.Trigger, error) {
	rows, err := d.db.Query(`
		SELECT t.name, OBJECT_NAME(t.parent_id),
		       te.type_desc, t.is_instead_of_trigger,
		       m.definition
		FROM sys.triggers t
		JOIN sys.trigger_events te ON te.object_id = t.object_id
		JOIN sys.sql_modules m ON m.object_id = t.object_id
		WHERE t.parent_class = 1
		ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []schema.Trigger
	for rows.Next() {
		var tr schema.Trigger
		var isInsteadOf bool
		if err := rows.Scan(&tr.Name, &tr.Table, &tr.Event, &isInsteadOf, &tr.Body); err != nil {
			return nil, err
		}
		if isInsteadOf {
			tr.Timing = "INSTEAD OF"
		} else {
			tr.Timing = "AFTER"
		}
		triggers = append(triggers, tr)
	}
	return triggers, rows.Err()
}
