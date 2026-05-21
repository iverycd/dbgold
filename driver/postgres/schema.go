package postgres

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
	full.Sequences, _ = d.extractSequences()

	return full, nil
}

func (d *Driver) listTables() ([]string, error) {
	rows, err := d.db.Query(`
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public'
		ORDER BY tablename`)
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
		SELECT c.column_name, c.udt_name, c.is_nullable, c.column_default,
		       CASE WHEN pk.column_name IS NOT NULL THEN true ELSE false END,
		       CASE WHEN c.column_default LIKE 'nextval%' THEN true ELSE false END
		FROM information_schema.columns c
		LEFT JOIN (
			SELECT ku.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage ku
			  ON tc.constraint_name = ku.constraint_name
			WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_name = $1
		) pk ON pk.column_name = c.column_name
		WHERE c.table_name = $1 AND c.table_schema = 'public'
		ORDER BY c.ordinal_position`, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []schema.Column
	for rows.Next() {
		var c schema.Column
		var isNullable string
		var defaultVal *string
		if err := rows.Scan(&c.Name, &c.Type, &isNullable, &defaultVal, &c.PrimaryKey, &c.AutoIncrement); err != nil {
			return nil, err
		}
		c.Nullable = isNullable == "YES"
		c.Default = defaultVal
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func (d *Driver) extractIndexes(tableName string) ([]schema.Index, error) {
	rows, err := d.db.Query(`
		SELECT i.relname, a.attname, ix.indisunique
		FROM pg_index ix
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(ix.indkey)
		WHERE t.relname = $1 AND NOT ix.indisprimary
		ORDER BY i.relname, a.attnum`, tableName)
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
		SELECT tc.constraint_name, kcu.column_name,
		       ccu.table_name, ccu.column_name,
		       rc.delete_rule, rc.update_rule
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name = tc.constraint_name
		JOIN information_schema.referential_constraints rc
		  ON rc.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_name = $1
		ORDER BY tc.constraint_name, kcu.ordinal_position`, tableName)
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
		SELECT tc.constraint_name, tc.constraint_type,
		       pg_get_constraintdef(c.oid)
		FROM information_schema.table_constraints tc
		JOIN pg_constraint c ON c.conname = tc.constraint_name
		WHERE tc.table_name = $1
		  AND tc.constraint_type IN ('CHECK', 'UNIQUE')
		ORDER BY tc.constraint_name`, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var constraints []schema.Constraint
	for rows.Next() {
		var c schema.Constraint
		if err := rows.Scan(&c.Name, &c.Type, &c.Def); err != nil {
			return nil, err
		}
		constraints = append(constraints, c)
	}
	return constraints, rows.Err()
}

func (d *Driver) extractViews() ([]schema.View, error) {
	rows, err := d.db.Query(`
		SELECT table_name, view_definition
		FROM information_schema.views
		WHERE table_schema = 'public'
		ORDER BY table_name`)
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
		SELECT trigger_name, event_object_table, event_manipulation,
		       action_timing, action_statement
		FROM information_schema.triggers
		WHERE trigger_schema = 'public'
		ORDER BY trigger_name`)
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

func (d *Driver) extractSequences() ([]schema.Sequence, error) {
	rows, err := d.db.Query(`
		SELECT sequence_name, start_value, increment, minimum_value, maximum_value
		FROM information_schema.sequences
		WHERE sequence_schema = 'public'
		ORDER BY sequence_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []schema.Sequence
	for rows.Next() {
		var s schema.Sequence
		if err := rows.Scan(&s.Name, &s.Start, &s.Increment, &s.MinValue, &s.MaxValue); err != nil {
			return nil, err
		}
		seqs = append(seqs, s)
	}
	return seqs, rows.Err()
}
