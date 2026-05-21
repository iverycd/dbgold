package oracle

import (
	"dbgold/schema"
	"fmt"
	"strings"
)

func (d *Driver) ExtractSchema(dbName string) (*schema.Schema, error) {
	if d.db == nil {
		return nil, fmt.Errorf("not connected")
	}
	owner := strings.ToUpper(dbName)
	s := &schema.Schema{Name: dbName}

	tables, err := d.listTables(owner)
	if err != nil {
		return nil, err
	}
	for _, tableName := range tables {
		t, err := d.extractTable(owner, tableName)
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
	owner := strings.ToUpper(dbName)
	base, err := d.ExtractSchema(dbName)
	if err != nil {
		return nil, err
	}
	full := &schema.FullSchema{Schema: *base}
	full.Views, _ = d.extractViews(owner)
	full.Triggers, _ = d.extractTriggers(owner)
	full.Sequences, _ = d.extractSequences(owner)
	return full, nil
}

func (d *Driver) listTables(owner string) ([]string, error) {
	rows, err := d.db.Query(
		`SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER = :1 ORDER BY TABLE_NAME`, owner)
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

func (d *Driver) extractTable(owner, tableName string) (*schema.Table, error) {
	t := &schema.Table{Name: tableName}
	var err error
	t.Columns, err = d.extractColumns(owner, tableName)
	if err != nil {
		return nil, err
	}
	t.Indexes, err = d.extractIndexes(owner, tableName)
	if err != nil {
		return nil, err
	}
	t.ForeignKeys, err = d.extractForeignKeys(owner, tableName)
	if err != nil {
		return nil, err
	}
	t.Constraints, err = d.extractConstraints(owner, tableName)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (d *Driver) extractColumns(owner, tableName string) ([]schema.Column, error) {
	rows, err := d.db.Query(`
		SELECT c.COLUMN_NAME, c.DATA_TYPE, c.NULLABLE, c.DATA_DEFAULT,
		       CASE WHEN p.COLUMN_NAME IS NOT NULL THEN 1 ELSE 0 END,
		       CASE WHEN c.IDENTITY_COLUMN = 'YES' THEN 1 ELSE 0 END
		FROM ALL_TAB_COLUMNS c
		LEFT JOIN (
			SELECT cc.COLUMN_NAME FROM ALL_CONSTRAINTS ac
			JOIN ALL_CONS_COLUMNS cc ON ac.CONSTRAINT_NAME = cc.CONSTRAINT_NAME AND ac.OWNER = cc.OWNER
			WHERE ac.CONSTRAINT_TYPE = 'P' AND ac.OWNER = :1 AND ac.TABLE_NAME = :2
		) p ON p.COLUMN_NAME = c.COLUMN_NAME
		WHERE c.OWNER = :1 AND c.TABLE_NAME = :2
		ORDER BY c.COLUMN_ID`, owner, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []schema.Column
	for rows.Next() {
		var c schema.Column
		var nullable string
		var isPK, isAutoInc int
		var defaultVal *string
		if err := rows.Scan(&c.Name, &c.Type, &nullable, &defaultVal, &isPK, &isAutoInc); err != nil {
			return nil, err
		}
		c.Nullable = nullable == "Y"
		c.Default = defaultVal
		c.PrimaryKey = isPK == 1
		c.AutoIncrement = isAutoInc == 1
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func (d *Driver) extractIndexes(owner, tableName string) ([]schema.Index, error) {
	rows, err := d.db.Query(`
		SELECT i.INDEX_NAME, ic.COLUMN_NAME, i.UNIQUENESS
		FROM ALL_INDEXES i
		JOIN ALL_IND_COLUMNS ic ON ic.INDEX_NAME = i.INDEX_NAME AND ic.INDEX_OWNER = i.OWNER
		WHERE i.OWNER = :1 AND i.TABLE_NAME = :2
		  AND i.INDEX_NAME NOT IN (
		      SELECT CONSTRAINT_NAME FROM ALL_CONSTRAINTS
		      WHERE CONSTRAINT_TYPE = 'P' AND OWNER = :1 AND TABLE_NAME = :2
		  )
		ORDER BY i.INDEX_NAME, ic.COLUMN_POSITION`, owner, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idxMap := map[string]*schema.Index{}
	var order []string
	for rows.Next() {
		var idxName, colName, uniqueness string
		if err := rows.Scan(&idxName, &colName, &uniqueness); err != nil {
			return nil, err
		}
		if _, exists := idxMap[idxName]; !exists {
			idxMap[idxName] = &schema.Index{Name: idxName, Unique: uniqueness == "UNIQUE"}
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

func (d *Driver) extractForeignKeys(owner, tableName string) ([]schema.ForeignKey, error) {
	rows, err := d.db.Query(`
		SELECT ac.CONSTRAINT_NAME, acc.COLUMN_NAME,
		       ac2.TABLE_NAME, acc2.COLUMN_NAME,
		       ac.DELETE_RULE
		FROM ALL_CONSTRAINTS ac
		JOIN ALL_CONS_COLUMNS acc ON ac.CONSTRAINT_NAME = acc.CONSTRAINT_NAME AND ac.OWNER = acc.OWNER
		JOIN ALL_CONSTRAINTS ac2 ON ac.R_CONSTRAINT_NAME = ac2.CONSTRAINT_NAME AND ac.R_OWNER = ac2.OWNER
		JOIN ALL_CONS_COLUMNS acc2 ON ac2.CONSTRAINT_NAME = acc2.CONSTRAINT_NAME AND ac2.OWNER = acc2.OWNER
		  AND acc.POSITION = acc2.POSITION
		WHERE ac.CONSTRAINT_TYPE = 'R' AND ac.OWNER = :1 AND ac.TABLE_NAME = :2
		ORDER BY ac.CONSTRAINT_NAME, acc.POSITION`, owner, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fkMap := map[string]*schema.ForeignKey{}
	var order []string
	for rows.Next() {
		var name, col, refTable, refCol, delRule string
		if err := rows.Scan(&name, &col, &refTable, &refCol, &delRule); err != nil {
			return nil, err
		}
		if _, exists := fkMap[name]; !exists {
			fkMap[name] = &schema.ForeignKey{Name: name, RefTable: refTable, OnDelete: delRule}
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

func (d *Driver) extractConstraints(owner, tableName string) ([]schema.Constraint, error) {
	rows, err := d.db.Query(`
		SELECT CONSTRAINT_NAME, CONSTRAINT_TYPE, SEARCH_CONDITION
		FROM ALL_CONSTRAINTS
		WHERE OWNER = :1 AND TABLE_NAME = :2
		  AND CONSTRAINT_TYPE IN ('C', 'U')
		ORDER BY CONSTRAINT_NAME`, owner, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var constraints []schema.Constraint
	for rows.Next() {
		var c schema.Constraint
		var ctype string
		if err := rows.Scan(&c.Name, &ctype, &c.Def); err != nil {
			return nil, err
		}
		if ctype == "C" {
			c.Type = "CHECK"
		} else {
			c.Type = "UNIQUE"
		}
		constraints = append(constraints, c)
	}
	return constraints, rows.Err()
}

func (d *Driver) extractViews(owner string) ([]schema.View, error) {
	rows, err := d.db.Query(
		`SELECT VIEW_NAME, TEXT FROM ALL_VIEWS WHERE OWNER = :1 ORDER BY VIEW_NAME`, owner)
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

func (d *Driver) extractTriggers(owner string) ([]schema.Trigger, error) {
	rows, err := d.db.Query(`
		SELECT TRIGGER_NAME, TABLE_NAME, TRIGGERING_EVENT,
		       TRIGGER_TYPE, TRIGGER_BODY
		FROM ALL_TRIGGERS WHERE OWNER = :1 ORDER BY TRIGGER_NAME`, owner)
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

func (d *Driver) extractSequences(owner string) ([]schema.Sequence, error) {
	rows, err := d.db.Query(`
		SELECT SEQUENCE_NAME, MIN_VALUE, INCREMENT_BY, LAST_NUMBER
		FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER = :1 ORDER BY SEQUENCE_NAME`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []schema.Sequence
	for rows.Next() {
		var s schema.Sequence
		if err := rows.Scan(&s.Name, &s.MinValue, &s.Increment, &s.Start); err != nil {
			return nil, err
		}
		seqs = append(seqs, s)
	}
	return seqs, rows.Err()
}
