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
	if err := r.populateMaterializedEmptyList(t, p); err != nil {
		return err
	}
	hasPK := false
	for _, c := range p.Constraints {
		hasPK = hasPK || c.Table == t.Name && c.Kind == "P"
	}
	if !hasPK {
		return nil
	}
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
			if p.Columns[i].ValueSQL == "" {
				p.Columns[i].ValueSQL = value
			}
		}
	}
	return nil
}

func (r *catalogReader) populateMaterializedEmptyList(t Table, p *MaterializedEmptyTouch) error {
	tokens, err := nativeTokens(p.DefinitionSQL)
	if err != nil {
		return err
	}
	var keys []string
	for i := 0; i+3 < len(tokens); i++ {
		if tokens[i].kind != 'i' || !strings.EqualFold(tokens[i].value, "PARTITION") || !strings.EqualFold(tokens[i+1].value, "BY") || !strings.EqualFold(tokens[i+2].value, "LIST") || tokens[i+3].value != "(" {
			continue
		}
		for j := i + 4; j < len(tokens) && tokens[j].value != ")"; j++ {
			if tokens[j].kind == 'q' {
				keys = append(keys, tokens[j].value)
			} else if tokens[j].kind == 'i' {
				keys = append(keys, strings.ToUpper(tokens[j].value))
			}
		}
		break
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(r.ctx, `SELECT HIGH_VALUE FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY PARTITION_POSITION`, r.schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	var tuple []string
	for rows.Next() {
		var value sql.NullString
		if err = rows.Scan(&value); err != nil {
			return err
		}
		if !value.Valid || strings.EqualFold(strings.TrimSpace(value.String), "DEFAULT") {
			continue
		}
		tuple, err = materializedEmptyListTuple(value.String, len(keys))
		if err != nil {
			return err
		}
		break
	}
	if err = rows.Err(); err != nil || len(tuple) == 0 {
		return err // A DEFAULT-only list accepts the ordinary NULL/typed seeds.
	}
	types := map[string]Column{}
	for _, c := range t.Columns {
		types[c.Name] = c
	}
	for i, key := range keys {
		c, found := types[key]
		if !found || !c.Writable {
			return fmt.Errorf("LIST 分区列 %s 缺少原生可写类型", key)
		}
		typeSQL := c.SQLType
		if typeSQL == "" {
			typeSQL = c.DataType
		}
		for j := range p.Columns {
			if p.Columns[j].Name == key {
				p.Columns[j].ValueSQL = "CAST(" + tuple[i] + " AS " + typeSQL + ")"
			}
		}
	}
	return nil
}

// HIGH_VALUE is native constant SQL, potentially containing quoted commas or
// a multi-column list of tuples. Freeze the first complete tuple without
// splitting any literal, CAST, or interval expression.
func materializedEmptyListTuple(high string, width int) ([]string, error) {
	tokens, err := nativeTokens(high)
	if err != nil || len(tokens) == 0 || width < 1 {
		return nil, fmt.Errorf("LIST 分区边界不是完整原生常量")
	}
	start, depth, baseline := 0, 0, 0
	if tokens[0].value == "(" {
		start, depth, baseline = 1, 1, 1
	}
	var parts []string
	for i := start; i < len(tokens); i++ {
		token := tokens[i]
		if token.kind != 'p' {
			continue
		}
		if token.value == "(" {
			depth++
		}
		if token.value == "," && depth == baseline || token.value == ")" && depth == baseline {
			if i == start {
				return nil, fmt.Errorf("LIST 分区边界包含空值表达式")
			}
			parts = append(parts, high[tokens[start].start:tokens[i-1].end])
			if len(parts) == width {
				return parts, nil
			}
			start = i + 1
		}
		if token.value == ")" {
			depth--
			if depth < baseline {
				break
			}
		}
	}
	if start < len(tokens) && len(parts)+1 == width {
		parts = append(parts, high[tokens[start].start:tokens[len(tokens)-1].end])
		return parts, nil
	}
	return nil, fmt.Errorf("LIST 分区边界元组列数与分区键不一致")
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
