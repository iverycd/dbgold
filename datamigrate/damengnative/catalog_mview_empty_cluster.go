package damengnative

import (
	"database/sql"
	"fmt"
	"strings"
)

// A clustered primary key cannot be disabled or made nullable. Keep that
// physical key intact and freeze non-NULL, typed values for its temporary row.
// Prefer one actual historical key tuple when the immutable native log has it;
// besides unusual scalar keys this preserves LIST-partition routing.
func (r *catalogReader) populateMaterializedEmptyCluster(t Table, p *MaterializedEmptyTouch) error {
	rows, err := r.db.QueryContext(r.ctx, `SELECT c.CONSTRAINT_NAME FROM ALL_CONSTRAINTS c JOIN ALL_INDEXES i ON i.OWNER=c.INDEX_OWNER AND i.INDEX_NAME=c.INDEX_NAME WHERE c.OWNER=? AND c.TABLE_NAME=? AND c.CONSTRAINT_TYPE='P' AND i.INDEX_TYPE='CLUSTER'`, r.schema, t.Name)
	if err != nil {
		return err
	}
	clustered := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		clustered[name] = true
	}
	iteration := rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if iteration != nil {
		return iteration
	}
	if len(clustered) == 0 {
		return nil
	}
	var key []string
	for i := range p.Constraints {
		c := &p.Constraints[i]
		if c.Table == t.Name && c.Kind == "P" && clustered[c.Name] {
			c.Clustered = true
			key = append(key, c.Columns...)
		}
	}
	if len(key) == 0 {
		return fmt.Errorf("空基表 %s 的聚集主键不在冻结约束清单中", t.Name)
	}
	columns := map[string]Column{}
	for _, c := range t.Columns {
		columns[c.Name] = c
	}
	values := map[string]string{}
	for _, name := range key {
		c, found := columns[name]
		if !found || !c.Writable {
			return fmt.Errorf("聚集主键列 %s 没有原生可写类型", name)
		}
		values[name], err = materializedEmptyClusterValue(c, nil)
		if err != nil {
			return err
		}
	}
	for _, object := range r.snapshot.Objects {
		if object.Kind != "materialized_view_log" || object.Managed || object.Table != t.Name {
			continue
		}
		physical := r.materializedLogDDLName(t.Name)
		logColumns, e := r.historyColumns(physical)
		if e != nil {
			return e
		}
		available := map[string]bool{}
		for _, c := range logColumns {
			available[c.Name] = true
		}
		var expressions, predicates []string
		for _, name := range key {
			if !available[name] {
				expressions = nil
				break
			}
			expressions = append(expressions, materializedEmptyClusterRead(columns[name]))
			predicates = append(predicates, Quote(name)+" IS NOT NULL")
		}
		if len(expressions) == 0 {
			continue
		}
		text := make([]sql.NullString, len(key))
		dest := make([]any, len(key))
		for i := range dest {
			dest[i] = &text[i]
		}
		e = r.db.QueryRowContext(r.ctx, "SELECT "+strings.Join(expressions, ",")+" FROM "+Qualified(r.schema, physical)+" WHERE "+strings.Join(predicates, " AND ")+" AND ROWNUM=1").Scan(dest...)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return e
		}
		for i, name := range key {
			if !text[i].Valid {
				return fmt.Errorf("聚集主键的历史种子意外为空")
			}
			v, e := materializedEmptyClusterValue(columns[name], &text[i].String)
			if e != nil {
				return e
			}
			values[name] = v
		}
		break
	}
	for i := range p.Columns {
		if value, found := values[p.Columns[i].Name]; found {
			p.Columns[i].ValueSQL = value
		}
	}
	return nil
}

func materializedEmptyClusterRead(c Column) string {
	q := Quote(c.Name)
	kind := strings.ToUpper(strings.TrimSpace(c.DataType))
	switch {
	case strings.Contains(kind, "BINARY"), kind == "RAW":
		return "RAWTOHEX(" + q + ")"
	case kind == "ROWID":
		return "ROWIDTOCHAR(" + q + ")"
	case kind == "DATE":
		return "TO_CHAR(" + q + ",'YYYY-MM-DD')"
	case strings.Contains(kind, "TIMESTAMP"), strings.Contains(kind, "DATETIME"):
		format := "YYYY-MM-DD HH24:MI:SS.FF6"
		if strings.Contains(kind, "TIME ZONE") {
			format += " TZH:TZM"
		}
		return "TO_CHAR(" + q + ",'" + format + "')"
	case strings.HasPrefix(kind, "TIME"):
		return "TO_CHAR(" + q + ",'HH24:MI:SS.FF6')"
	default:
		return "CAST(" + q + " AS VARCHAR(32767))"
	}
}

func materializedEmptyClusterValue(c Column, historical *string) (string, error) {
	typeSQL := strings.TrimSpace(c.SQLType)
	if typeSQL == "" {
		typeSQL = strings.TrimSpace(c.DataType)
	}
	if typeSQL == "" {
		return "", fmt.Errorf("聚集主键列 %s 缺少原生 SQL 类型", c.Name)
	}
	kind := strings.ToUpper(strings.TrimSpace(c.DataType))
	value := "'0'"
	if historical != nil {
		value = "'" + strings.ReplaceAll(*historical, "'", "''") + "'"
	}
	switch {
	case strings.Contains(kind, "BINARY"), kind == "RAW":
		hex := "00"
		if historical != nil {
			hex = *historical
		}
		for _, ch := range hex {
			if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
				return "", fmt.Errorf("聚集二进制主键种子不是十六进制")
			}
		}
		value = "0x" + hex
	case kind == "ROWID":
		if historical != nil {
			return "CHARTOROWID(" + value + ")", nil
		}
		value = "1"
	case kind == "DATE":
		if historical == nil {
			value = "'2000-01-01'"
		}
	case strings.Contains(kind, "TIMESTAMP"), strings.Contains(kind, "DATETIME"):
		if historical == nil {
			value = "'2000-01-01 00:00:00'"
			if strings.Contains(kind, "TIME ZONE") {
				value = "'2000-01-01 00:00:00 +00:00'"
			}
		}
	case strings.HasPrefix(kind, "TIME"):
		if historical == nil {
			value = "'00:00:00'"
		}
	case strings.HasPrefix(kind, "INTERVAL"):
		if historical == nil {
			if strings.Contains(kind, "YEAR") {
				value = "'0-0'"
			} else {
				value = "'0 00:00:00'"
			}
		}
	}
	return "CAST(" + value + " AS " + typeSQL + ")", nil
}
