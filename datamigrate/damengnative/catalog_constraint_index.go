package damengnative

import (
	"fmt"
	"sort"
	"strings"
)

// DM8 TABLE/CONSTRAINT metadata omits both the index selected by USING INDEX
// and the validation flag of non-FK constraints. Freeze those facts from the
// catalog before replay. A reused key is installed after its original index;
// enabled NOVALIDATE constraints are installed after copying historical rows.
// Every unit remains owned by the table, so recovery preserves their order.
func (r *catalogReader) freezeTableKeyAndValidationPlan(table, ddl string) (string, []Unit, []Unit, error) {
	type selectedConstraint struct {
		object Object
		ddl    string
		index  *Object
	}
	var selected []selectedConstraint
	for _, object := range r.snapshot.Objects {
		if object.Kind != "constraint" || object.Table != table || object.ConstraintType == "R" {
			continue
		}
		var index *Object
		if object.ConstraintIndex != "" && (object.ConstraintType == "P" || object.ConstraintType == "U") {
			for i := range r.snapshot.Objects {
				candidate := &r.snapshot.Objects[i]
				if candidate.Kind == "index" && candidate.Name == object.ConstraintIndex && candidate.Table == table && !candidate.Generated {
					index = candidate
					break
				}
			}
		}
		if index == nil && !object.ConstraintNoValidate && !(object.Generated && object.Status == "DISABLED") {
			continue
		}
		definition, err := r.getDDL("CONSTRAINT", object.Name)
		definition = repairGeneratedConstraintIdentity(definition, object)
		if err != nil {
			return "", nil, nil, fmt.Errorf("约束 %s 的完整原生定义不可读取: %w", object.Name, err)
		}
		selected = append(selected, selectedConstraint{object: object, ddl: definition, index: index})
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].object.Name < selected[j].object.Name })
	var foundations, delayed []Unit
	indexes := map[string]bool{}
	for _, index := range r.snapshot.Objects {
		if index.Kind != "index" || index.Table != table || !index.Managed || index.Generated {
			continue
		}
		ownedByKey := false
		for _, constraint := range r.snapshot.Objects {
			if constraint.Kind == "constraint" && constraint.Table == table && constraint.ConstraintIndex == index.Name && (constraint.ConstraintType == "P" || constraint.ConstraintType == "U") {
				ownedByKey = true
				break
			}
		}
		if ownedByKey {
			continue
		}
		definition, err := r.getDDL("INDEX", index.Name)
		if err != nil {
			return "", nil, nil, fmt.Errorf("用户聚集索引 %s 的原生定义不可读取: %w", index.Name, err)
		}
		ddl, err = removeNativeTableClusterKey(ddl, r.schema, table)
		if err != nil {
			return "", nil, nil, err
		}
		foundations = append(foundations, Unit{SQL: definition, Phase: "before_data"})
		indexes[index.Name] = true
	}
	for _, item := range selected {
		var err error
		ddl, err = removeNativeTableConstraint(ddl, item.object, item.ddl)
		if err != nil {
			return "", nil, nil, err
		}
		definition := item.ddl
		if item.index != nil {
			if !indexes[item.index.Name] {
				indexDDL, e := r.getDDL("INDEX", item.index.Name)
				if e != nil {
					return "", nil, nil, fmt.Errorf("约束 %s 使用的原生索引 %s 不可读取: %w", item.object.Name, item.index.Name, e)
				}
				foundations = append(foundations, Unit{SQL: indexDDL, Phase: "before_data"})
				indexes[item.index.Name] = true
			}
			definition, err = constraintUsingIndex(definition, r.schema, item.index.Name)
			if err != nil {
				return "", nil, nil, err
			}
		}
		definition, err = constraintFrozenState(definition, item.object)
		if err != nil {
			return "", nil, nil, err
		}
		definition += " " + r.frozenConstraintMarker(item.object)
		if item.object.ConstraintNoValidate {
			delayed = append(delayed, Unit{SQL: definition, Phase: "after_data"})
		} else {
			foundations = append(foundations, Unit{SQL: definition, Phase: "before_data"})
		}
	}
	return ddl, foundations, delayed, nil
}

func constraintUsingIndex(ddl, owner, index string) (string, error) {
	ddl, err := constraintWithoutState(ddl)
	if err != nil {
		return "", err
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	depth := 0
	for i, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
		} else if depth == 0 && t.kind == 'i' && strings.EqualFold(t.value, "USING") && i+1 < len(ts) && ts[i+1].kind == 'i' && strings.EqualFold(ts[i+1].value, "INDEX") {
			// A future metadata build may preserve this clause. The catalog
			// remains authoritative; replace only its terminal index option.
			return strings.TrimSpace(ddl[:t.start]) + " USING INDEX " + Qualified(owner, index), nil
		}
	}
	return ddl + " USING INDEX " + Qualified(owner, index), nil
}

// removeNativeTableConstraint splices one top-level native constraint out of
// CREATE TABLE. Named constraints match their exact identifier. Generated names
// omitted by TABLE metadata match the native constraint body instead; values,
// quoting, expressions, storage and every unrelated clause remain untouched.
func removeNativeTableConstraint(ddl string, object Object, definition string) (string, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	open, close, depth := -1, -1, 0
	for i, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			if open < 0 {
				open = i
			}
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
			if depth == 0 && open >= 0 {
				close = i
				break
			}
		}
	}
	if open < 0 || close < 0 {
		return "", fmt.Errorf("表 %s 的原生 DDL 缺少完整列定义", object.Table)
	}
	type piece struct{ start, end int }
	var pieces []piece
	start, depth := ts[open].end, 0
	for _, t := range ts[open+1 : close] {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
		} else if depth == 0 && t.kind == 'p' && t.value == "," {
			pieces = append(pieces, piece{start, t.start})
			start = t.end
		}
	}
	pieces = append(pieces, piece{start, ts[close].start})
	body, err := nativeConstraintBody(definition, true)
	if err != nil {
		return "", err
	}
	for i, p := range pieces {
		part := ddl[p.start:p.end]
		pt, e := nativeTokens(part)
		if e != nil || len(pt) == 0 {
			continue
		}
		match := false
		if len(pt) > 1 && pt[0].kind == 'i' && strings.EqualFold(pt[0].value, "CONSTRAINT") {
			name := pt[1].value
			if pt[1].kind == 'i' {
				name = strings.ToUpper(name)
			}
			match = name == object.Name
		} else if object.Generated {
			candidate, e := nativeConstraintBody(part, false)
			match = e == nil && candidate == body
		}
		if !match {
			continue
		}
		if i+1 < len(pieces) {
			return ddl[:p.start] + ddl[pieces[i+1].start:], nil
		}
		if i > 0 {
			return ddl[:pieces[i-1].end] + ddl[p.end:], nil
		}
		return "", fmt.Errorf("表 %s 的原生定义未包含可保留列", object.Table)
	}
	return "", fmt.Errorf("约束 %s 的原生定义与表级清单不一致", object.Name)
}

func nativeConstraintBody(ddl string, alter bool) (string, error) {
	ddl, err := constraintWithoutState(ddl)
	if err != nil {
		return "", err
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	start := 0
	if alter {
		start = -1
		for i, t := range ts {
			if t.kind == 'i' && strings.EqualFold(t.value, "ADD") {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return "", fmt.Errorf("约束原生定义缺少 ADD")
		}
	}
	if start+1 < len(ts) && ts[start].kind == 'i' && strings.EqualFold(ts[start].value, "CONSTRAINT") {
		start += 2
	}
	var out strings.Builder
	for _, t := range ts[start:] {
		v := t.value
		if t.kind == 'i' {
			v = strings.ToUpper(v)
		}
		out.WriteByte(t.kind)
		out.WriteString(v)
		out.WriteByte(0)
	}
	return out.String(), nil
}

// Some DM8 builds omit generated names and emit mixed-case CHECK names
// without quotes. Freeze the catalog identity before state normalization; the
// caller removes only proven generated names from the executable unit.
func repairGeneratedConstraintIdentity(ddl string, object Object) string {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return ddl
	}
	for i := 0; i+2 < len(ts); i++ {
		if ts[i].kind != 'i' || !strings.EqualFold(ts[i].value, "ADD") || ts[i+1].kind != 'i' || !strings.EqualFold(ts[i+1].value, "CONSTRAINT") {
			continue
		}
		next := ts[i+2]
		if next.kind == 'i' && (strings.EqualFold(next.value, "CHECK") || strings.EqualFold(next.value, "UNIQUE") || strings.EqualFold(next.value, "PRIMARY") || strings.EqualFold(next.value, "FOREIGN") || strings.EqualFold(next.value, "NOT") || strings.EqualFold(next.value, "CLUSTER")) {
			return ddl[:next.start] + Quote(object.Name) + " " + ddl[next.start:]
		}
		return ddl[:next.start] + Quote(object.Name) + ddl[next.end:]
	}
	return ddl
}

// TABLE metadata reduces a user-named standalone clustered index to an
// anonymous CLUSTER [UNIQUE] KEY. Remove that generated declaration before
// replaying the exact native CREATE CLUSTER INDEX; retaining it would make the
// named CREATE fail with "this column list is already indexed".
func removeNativeTableClusterKey(ddl, owner, table string) (string, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	depth := 0
	for i, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			depth++
			continue
		}
		if t.kind == 'p' && t.value == ")" {
			depth--
			continue
		}
		if depth != 1 || t.kind != 'i' || !strings.EqualFold(t.value, "CLUSTER") || i+1 >= len(ts) {
			continue
		}
		j := i + 1
		if strings.EqualFold(ts[j].value, "UNIQUE") {
			j++
		}
		if j >= len(ts) || !strings.EqualFold(ts[j].value, "KEY") {
			continue
		}
		end, nested := j+1, 0
		for ; end < len(ts); end++ {
			if ts[end].kind == 'p' && ts[end].value == "(" {
				nested++
			} else if ts[end].kind == 'p' && ts[end].value == ")" {
				if nested == 0 {
					break
				}
				nested--
			} else if nested == 0 && ts[end].kind == 'p' && ts[end].value == "," {
				break
			}
		}
		if end < len(ts) {
			definition := "ALTER TABLE " + Qualified(owner, table) + " ADD " + ddl[t.start:ts[end].start]
			return removeNativeTableConstraint(ddl, Object{Table: table, Generated: true}, definition)
		}
	}
	return "", fmt.Errorf("表 %s 的用户聚集索引缺少可确认的原生 CLUSTER KEY 声明", table)
}

// A stable server-frozen marker distinguishes duplicate generated ADD units in
// the execution ledger. SQL token/definition comparison ignores the comment.
func (r *catalogReader) frozenConstraintMarker(object Object) string {
	id := object.ID
	if id == "" {
		id = ObjectID(r.schema, "constraint", object.Name)
	}
	return "/* DBGOLD_CONSTRAINT " + id + " */"
}

func (r *catalogReader) hasFrozenConstraintUnit(object Object, units []Unit) bool {
	marker := r.frozenConstraintMarker(object)
	for _, unit := range units {
		if strings.HasSuffix(strings.TrimSpace(unit.SQL), marker) {
			return true
		}
	}
	return false
}
