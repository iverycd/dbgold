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
	// DM can retain an identity attribute in physical interval children even
	// after DROP IDENTITY on the parent, making SET NULL fail with -2720.
	// A typed explicit constant avoids changing this mandatory NOT NULL while
	// DROP/ADD IDENTITY still prevents consuming its original continuation.
	for _, c := range t.Columns {
		if !c.Identity || !c.Writable || c.SQLType == "" && c.DataType == "" {
			continue
		}
		value, err := materializedEmptyClusterValue(c, nil)
		if err != nil {
			return err
		}
		for i := range p.Columns {
			if p.Columns[i].Name == c.Name && p.Columns[i].ValueSQL == "" {
				p.Columns[i].ValueSQL = value
			}
		}
	}
	hasPK := false
	for _, c := range p.Constraints {
		hasPK = hasPK || c.Table == t.Name && c.Kind == "P"
	}
	if !hasPK {
		return r.populateMaterializedEmptyPartition(t, p)
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
		return r.populateMaterializedEmptyPartition(t, p)
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
	return r.populateMaterializedEmptyPartition(t, p)
}

// Every candidate is resolved by the native read-only PARTITION FOR clause.
// This covers all declared partition levels and the actual INTERVAL layout,
// without reimplementing DM's collation, inclusive-boundary or NULL rules. An
// absent interval partition is rejected by SELECT rather than being created.
func (r *catalogReader) populateMaterializedEmptyPartition(t Table, p *MaterializedEmptyTouch) error {
	levels, err := materializedEmptyPartitionLevels(p.DefinitionSQL)
	if err != nil || len(levels) == 0 {
		return err
	}
	types := map[string]Column{}
	for _, c := range t.Columns {
		types[c.Name] = c
	}
	// A generated partition key is not an INSERT column. Retain the existing
	// empty-table NULL epoch instead of introducing a preflight rejection or
	// evaluating an arbitrary generation expression on the source. Preserve
	// the old top-level writable LIST seed when such a key has deeper levels.
	generatedKey := false
	for _, level := range levels {
		for _, name := range level.keys {
			c, found := types[name]
			if !found {
				return fmt.Errorf("分区列 %s 缺少原生类型", name)
			}
			generatedKey = generatedKey || !c.Writable
		}
	}
	if generatedKey {
		if levels[0].kind == "LIST" {
			for _, tuple := range levels[0].bounds {
				if len(tuple) != len(levels[0].keys) || strings.EqualFold(strings.TrimSpace(tuple[0]), "DEFAULT") {
					continue
				}
				for i, name := range levels[0].keys {
					if !types[name].Writable {
						continue
					}
					value := materializedEmptyPartitionValues(types[name], tuple[i], false)[0]
					for j := range p.Columns {
						if p.Columns[j].Name == name {
							p.Columns[j].ValueSQL = value
						}
					}
				}
				break
			}
		}
		return nil
	}
	var names []string
	options := map[string][]string{}
	appendOption := func(name, value string) {
		for _, old := range options[name] {
			if old == value {
				return
			}
		}
		options[name] = append(options[name], value)
	}
	for _, level := range levels {
		for i, name := range level.keys {
			c, found := types[name]
			if !found || !c.Writable {
				return fmt.Errorf("分区列 %s 缺少原生可写类型", name)
			}
			if _, seen := options[name]; !seen {
				names = append(names, name)
				options[name] = nil
			}
			// Prefer real LIST members and a RANGE upper bound's predecessor.
			// The ordinary zero seed may have no partition (for example <-5).
			for _, tuple := range level.bounds {
				if i >= len(tuple) || strings.EqualFold(strings.TrimSpace(tuple[i]), "MAXVALUE") || strings.EqualFold(strings.TrimSpace(tuple[i]), "DEFAULT") {
					continue
				}
				if !materializedEmptyPartitionConstant(tuple[i]) {
					continue // Never evaluate a user function while freezing routing.
				}
				for _, value := range materializedEmptyPartitionValues(c, tuple[i], level.kind == "RANGE") {
					appendOption(name, value)
				}
			}
		}
	}
	for _, name := range names {
		for _, c := range p.Columns {
			if c.Name == name && c.ValueSQL != "" {
				appendOption(name, c.ValueSQL)
			}
		}
		seed, e := materializedEmptyClusterValue(types[name], nil)
		if e != nil {
			return e
		}
		appendOption(name, seed)
	}
	// Partition columns can be reused at multiple levels. Enumerate assignments
	// by column, then supply the same frozen value to each native key position.
	values := map[string]string{}
	var last error
	var visit func(int) (bool, error)
	visit = func(index int) (bool, error) {
		if err := r.ctx.Err(); err != nil {
			return false, err
		}
		if index < len(names) {
			for _, value := range options[names[index]] {
				values[names[index]] = value
				ok, e := visit(index + 1)
				if e != nil || ok {
					return ok, e
				}
			}
			return false, nil
		}
		var keys, nonnull []string
		for _, level := range levels {
			for _, name := range level.keys {
				keys = append(keys, values[name])
			}
		}
		for _, name := range names {
			nonnull = append(nonnull, values[name]+" IS NOT NULL")
		}
		clause := "PARTITION"
		if len(levels) > 1 {
			clause = "SUBPARTITION"
		}
		query := "SELECT CASE WHEN " + strings.Join(nonnull, " AND ") + " THEN (SELECT COUNT(*) FROM " + Qualified(r.schema, t.Name) + " " + clause + " FOR(" + strings.Join(keys, ",") + ")) ELSE -1 END FROM DUAL"
		var count int64
		e := r.db.QueryRowContext(r.ctx, query).Scan(&count)
		if e != nil {
			last = e
			return false, nil
		}
		if count < 0 {
			return false, nil
		}
		if count != 0 {
			return false, fmt.Errorf("冻结为空的分区基表 %s 在只读路由校验时已有数据", t.Name)
		}
		for i := range p.Columns {
			if value, found := values[p.Columns[i].Name]; found {
				p.Columns[i].ValueSQL = value
			}
		}
		return true, nil
	}
	found, err := visit(0)
	if err != nil {
		return err
	}
	if !found {
		if last != nil {
			return fmt.Errorf("空基表 %s 的既有分区中没有可验证的临时键值: %w", t.Name, last)
		}
		return fmt.Errorf("空基表 %s 的既有分区中没有非 NULL 临时键值", t.Name)
	}
	return nil
}

// Native metadata contains only constants and the documented date constructors
// in partition bounds. Keep the expression typed; arithmetic overflow and empty
// string/NULL candidates are rejected by the read-only native route operation.
func materializedEmptyPartitionValues(c Column, boundary string, rangeKey bool) []string {
	typeSQL := strings.TrimSpace(c.SQLType)
	if typeSQL == "" {
		typeSQL = strings.TrimSpace(c.DataType)
	}
	cast := func(v string) string { return "CAST(" + v + " AS " + typeSQL + ")" }
	base := cast(boundary)
	if !rangeKey {
		return []string{base}
	}
	kind := strings.ToUpper(strings.TrimSpace(c.DataType))
	var values []string
	switch {
	case strings.Contains(kind, "CHAR"):
		values = []string{cast("CHR(0)"), cast("SUBSTR(" + base + ",1,LENGTH(" + base + ")-1)")}
	case kind == "DATE":
		values = []string{cast(base + "-1"), cast(base + "+1")}
	case strings.HasPrefix(kind, "TIME"), strings.Contains(kind, "DATETIME"):
		values = []string{cast(base + "-NUMTODSINTERVAL(0.000001,'SECOND')"), cast(base + "+NUMTODSINTERVAL(0.000001,'SECOND')"), cast(base + "-NUMTODSINTERVAL(1,'DAY')")}
	default:
		step := "1"
		if c.Scale > 0 && c.Scale <= 38 {
			step = "0." + strings.Repeat("0", int(c.Scale)-1) + "1"
		} else if c.Scale < 0 && c.Scale >= -38 {
			step = "1" + strings.Repeat("0", int(-c.Scale))
		}
		values = []string{cast(base + "-" + step), cast(base + "+" + step)}
		if kind == "REAL" || kind == "FLOAT" || kind == "DOUBLE" || strings.Contains(kind, "DOUBLE PRECISION") {
			values = append(values, cast(base+"-ABS("+base+")*0.000001"), cast(base+"+ABS("+base+")*0.000001"), cast(base+"-ABS("+base+")*0.000000000000001"), cast(base+"+ABS("+base+")*0.000000000000001"))
		}
	}
	return append(values, base)
}

// Bounds accepted by DM are persisted as literals or native date constructors.
// A function installed by a user must never be executed by read-only preflight,
// including one qualified by a schema or hidden in a constant-looking CAST.
func materializedEmptyPartitionConstant(text string) bool {
	ts, err := nativeTokens(text)
	if err != nil || len(ts) == 0 {
		return false
	}
	functions := map[string]bool{"CAST": true, "TO_DATE": true, "TO_DATETIME": true, "TO_TIMESTAMP": true, "TO_TIMESTAMP_TZ": true, "TO_NUMBER": true, "TO_CHAR": true, "CHR": true, "NCHR": true, "UNISTR": true, "HEXTORAW": true, "NUMTOYMINTERVAL": true, "NUMTODSINTERVAL": true}
	// Reject bare no-argument functions as well as function calls. Datatypes,
	// literal prefixes and CAST modifiers are the only non-call identifiers.
	words := map[string]bool{}
	for _, word := range strings.Fields("MAXVALUE DEFAULT NULL TRUE FALSE N X AS DATE TIMESTAMP DATETIME TIME INTERVAL WITH LOCAL ZONE YEAR TO MONTH DAY HOUR MINUTE SECOND PRECISION CHAR CHARACTER BYTE VARYING INT INTEGER BIGINT SMALLINT TINYINT BIT BOOLEAN BOOL REAL DOUBLE DECIMAL DEC NUMBER NUMERIC FLOAT BINARY_FLOAT BINARY_DOUBLE VARCHAR VARCHAR2 NCHAR NVARCHAR NVARCHAR2") {
		words[word] = true
	}
	for i, t := range ts {
		if t.kind == 'q' || t.kind == 'p' && (t.value == ";" || t.value == ":" || t.value == "?") {
			return false
		}
		if t.kind == 'p' && t.value == "." && i > 0 && ts[i-1].kind == 'i' {
			return false
		}
		if t.kind != 'i' {
			continue
		}
		name := strings.ToUpper(t.value)
		if functions[name] {
			if i+1 >= len(ts) || ts[i+1].value != "(" {
				return false
			}
			continue
		}
		if words[name] {
			if (name == "N" || name == "X") && (i+1 >= len(ts) || ts[i+1].kind != 's') {
				return false
			}
			continue
		}
		// The tokenizer retains punctuation separately from digits. Accept
		// only numeric exponent/hex suffixes contiguous with a numeric token.
		if t.start > 0 && text[t.start-1] >= '0' && text[t.start-1] <= '9' && len(name) > 0 {
			valid := name[0] == 'E' || name[0] == 'X' && text[t.start-1] == '0'
			for _, ch := range name[1:] {
				valid = valid && (ch >= '0' && ch <= '9' || name[0] == 'X' && ch >= 'A' && ch <= 'F')
			}
			if valid {
				continue
			}
		}
		return false
	}
	return true
}

type materializedEmptyPartitionLevel struct {
	kind   string
	keys   []string
	bounds [][]string
}

// GET_DDL preserves the complete templates and nested partition definitions.
// Using those definitions also avoids DM8's ALL_TAB_SUBPARTITIONS.TABLE_OWNER,
// which reports the owning user rather than a separately named schema.
func materializedEmptyPartitionLevels(ddl string) ([]materializedEmptyPartitionLevel, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return nil, err
	}
	var levels []materializedEmptyPartitionLevel
	match := func(i int, s string) bool {
		return i < len(ts) && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, s)
	}
	closeParen := func(open int) (int, error) {
		depth := 0
		for i := open; i < len(ts); i++ {
			if ts[i].kind == 'p' {
				if ts[i].value == "(" {
					depth++
				}
				if ts[i].value == ")" {
					depth--
					if depth == 0 {
						return i, nil
					}
				}
			}
		}
		return 0, fmt.Errorf("分区定义括号不完整")
	}
	for i := 0; i+3 < len(ts); i++ {
		if !(match(i, "PARTITION") || match(i, "SUBPARTITION")) || !match(i+1, "BY") {
			continue
		}
		if ts[i+3].value != "(" {
			return nil, fmt.Errorf("分区键定义不完整")
		}
		end, e := closeParen(i + 3)
		if e != nil {
			return nil, e
		}
		level := materializedEmptyPartitionLevel{kind: strings.ToUpper(ts[i+2].value)}
		for j := i + 4; j < end; j++ {
			if ts[j].kind == 'q' {
				level.keys = append(level.keys, ts[j].value)
			} else if ts[j].kind == 'i' {
				level.keys = append(level.keys, strings.ToUpper(ts[j].value))
			} else if ts[j].value != "," {
				return nil, fmt.Errorf("分区键不是原生列标识符")
			}
		}
		if len(level.keys) == 0 {
			return nil, fmt.Errorf("分区键为空")
		}
		levels = append(levels, level)
	}
	if len(levels) == 0 {
		return nil, nil
	}
	addBounds := func(level, open int) error {
		end, e := closeParen(open)
		if e != nil {
			return e
		}
		raw := ddl[ts[open].end:ts[end].start]
		parts, e := materializedEmptyPartitionParts(raw)
		if e != nil {
			return e
		}
		if levels[level].kind == "LIST" {
			for _, part := range parts {
				tuple := []string{part}
				if len(levels[level].keys) > 1 {
					pt, e := nativeTokens(part)
					if e != nil || len(pt) < 2 || pt[0].value != "(" || pt[len(pt)-1].value != ")" {
						return fmt.Errorf("LIST 分区边界不是完整原生元组")
					}
					tuple, e = materializedEmptyPartitionParts(part[pt[0].end:pt[len(pt)-1].start])
					if e != nil || len(tuple) != len(levels[level].keys) {
						return fmt.Errorf("LIST 分区边界元组列数与分区键不一致")
					}
				}
				levels[level].bounds = append(levels[level].bounds, tuple)
			}
		} else if len(parts) == len(levels[level].keys) {
			levels[level].bounds = append(levels[level].bounds, parts)
		} else {
			return fmt.Errorf("分区边界元组列数与分区键不一致")
		}
		return nil
	}
	// First collect template constants, assigning each to its BY declaration.
	active, root := -1, -1
	for i := 0; i < len(ts); i++ {
		if (match(i, "PARTITION") || match(i, "SUBPARTITION")) && match(i+1, "BY") {
			active++
			continue
		}
		if match(i, "PARTITION") && i+1 < len(ts) && (ts[i+1].kind == 'q' || ts[i+1].kind == 'i') && !match(i+1, "BY") {
			root = i - 1
			break
		}
		if match(i, "SUBPARTITION") && i+2 < len(ts) && !match(i+1, "BY") && !match(i+1, "TEMPLATE") && active >= 0 {
			for j := i + 2; j < len(ts) && !match(j, "SUBPARTITION"); j++ {
				if match(j, "VALUES") {
					for j++; j < len(ts) && ts[j].value != "("; j++ {
					}
					if j < len(ts) {
						if e := addBounds(active, j); e != nil {
							return nil, e
						}
					}
					break
				}
			}
		}
	}
	// Actual child definitions may override a template. Their list nesting gives
	// the level even when a partition name contains a keyword or parentheses.
	var walk func(int, int) error
	walk = func(open, level int) error {
		if level >= len(levels) {
			return fmt.Errorf("分区定义层数超出原生键清单")
		}
		end, e := closeParen(open)
		if e != nil {
			return e
		}
		for i := open + 1; i < end; i++ {
			if match(i, "VALUES") {
				for i++; i < end && ts[i].value != "("; i++ {
				}
				if i < end {
					if e = addBounds(level, i); e != nil {
						return e
					}
					i, e = closeParen(i)
					if e != nil {
						return e
					}
				}
				continue
			}
			if ts[i].kind == 'p' && ts[i].value == "(" {
				if match(i+1, "SUBPARTITION") {
					if e = walk(i, level+1); e != nil {
						return e
					}
				}
				i, e = closeParen(i)
				if e != nil {
					return e
				}
			}
		}
		return nil
	}
	if root >= 0 {
		if ts[root].value != "(" {
			return nil, fmt.Errorf("原生分区列表不完整")
		}
		if err = walk(root, 0); err != nil {
			return nil, err
		}
	}
	return levels, nil
}

func materializedEmptyPartitionParts(text string) ([]string, error) {
	ts, err := nativeTokens(text)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("分区边界为空")
	}
	start, depth := 0, 0
	var parts []string
	for i, t := range ts {
		if t.kind != 'p' {
			continue
		}
		if t.value == "(" {
			depth++
		}
		if t.value == ")" {
			depth--
		}
		if t.value == "," && depth == 0 {
			if start == i {
				return nil, fmt.Errorf("分区边界包含空表达式")
			}
			parts = append(parts, text[ts[start].start:ts[i-1].end])
			start = i + 1
		}
	}
	if start >= len(ts) || depth != 0 {
		return nil, fmt.Errorf("分区边界不是完整原生常量")
	}
	return append(parts, text[ts[start].start:ts[len(ts)-1].end]), nil
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
