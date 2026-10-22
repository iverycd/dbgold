package damengnative

import (
	"fmt"
	"sort"
	"strings"
)

// TABLE metadata omits both generated FK names and ENABLE NOVALIDATE on DM8.
// Read each FK's complete native definition and join its authoritative catalog
// state before any target action. This also permits a legitimate enabled
// NOVALIDATE FK whose existing data contains rows not present in the parent.
func (r *catalogReader) freezeTableForeignKeys(table string, count int) ([]string, error) {
	var constraints []Object
	for _, object := range r.snapshot.Objects {
		if object.Kind == "constraint" && object.Table == table && object.ConstraintType == "R" {
			constraints = append(constraints, object)
		}
	}
	if len(constraints) != count {
		return nil, fmt.Errorf("表 %s 的外键原生定义与目录清单数量不同", table)
	}
	sort.Slice(constraints, func(i, j int) bool { return constraints[i].Name < constraints[j].Name })
	var units []string
	for _, object := range constraints {
		ddl, err := r.getDDL("CONSTRAINT", object.Name)
		if err != nil {
			return nil, fmt.Errorf("外键 %s 的完整原生定义不可读取: %w", object.Name, err)
		}
		ddl, err = constraintFrozenState(ddl, object)
		if err != nil {
			return nil, err
		}
		units = append(units, ddl)
	}
	return units, nil
}

func constraintFrozenState(ddl string, object Object) (string, error) {
	ddl = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ddl), ";"))
	if object.Status != "ENABLED" && object.Status != "DISABLED" {
		return ddl, nil
	}
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", err
	}
	type span struct{ start, end int }
	var removes []span
	depth := 0
	for _, t := range ts {
		if t.kind == 'p' && t.value == "(" {
			depth++
		} else if t.kind == 'p' && t.value == ")" {
			depth--
		} else if depth == 0 && t.kind == 'i' && (strings.EqualFold(t.value, "ENABLE") || strings.EqualFold(t.value, "DISABLE") || strings.EqualFold(t.value, "VALIDATE") || strings.EqualFold(t.value, "NOVALIDATE")) {
			removes = append(removes, span{t.start, t.end})
		}
	}
	for i := len(removes) - 1; i >= 0; i-- {
		ddl = ddl[:removes[i].start] + ddl[removes[i].end:]
	}
	state := " ENABLE"
	if object.Status == "DISABLED" {
		state = " DISABLE"
	} else if object.ConstraintNoValidate {
		state += " NOVALIDATE"
	}
	return strings.TrimSpace(ddl) + state, nil
}
