package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

// A frozen table plan may own a separately named index and deferred native
// constraints. Reconcile those actual child objects, rather than treating the
// table's existence as proof that an individual CREATE/ADD completed.
func (w *DaMengNativeWriter) reconcileNativeOwnedUnit(ctx context.Context, object damengnative.Object, unit int, snapshot *damengnative.Snapshot) (handled, done, retry bool, err error) {
	if object.Kind != "table" || unit < 0 || unit >= len(object.Units) || snapshot == nil {
		return false, false, false, nil
	}
	sqlText := object.Units[unit].SQL
	tokens, err := nativeSQLTokens(sqlText)
	if err != nil {
		return false, false, false, err
	}
	if name, ok := nativeOwnedIndexHead(tokens, w.schema, object.Name); ok {
		var found *damengnative.Object
		for i := range snapshot.Objects {
			candidate := &snapshot.Objects[i]
			if candidate.Kind == "index" && candidate.Name == name && candidate.Table == object.Name {
				found = candidate
				break
			}
		}
		if found == nil {
			// A same-name index on another table is conflicting catalog state,
			// never a license for CREATE OR REPLACE to overwrite that object.
			for _, candidate := range snapshot.Objects {
				if candidate.Kind == "index" && candidate.Name == name {
					return true, false, false, nil
				}
			}
			return true, false, true, nil
		}
		var ddl sql.NullString
		if err = w.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('INDEX',?,?)`, name, w.schema).Scan(&ddl); err != nil {
			return true, false, false, err
		}
		return true, ddl.Valid && nativeDefinitionsEqual(sqlText, ddl.String), false, nil
	}
	shape, ok := nativeOwnedConstraintShape(tokens, w.schema, object.Name)
	if !ok {
		return false, false, false, nil
	}
	matched := 0
	for _, candidate := range snapshot.Objects {
		if shape.name == "" && !candidate.Generated {
			continue
		}
		if candidate.Kind != "constraint" || candidate.Table != object.Name || candidate.ConstraintType != shape.kind || shape.name != "" && candidate.Name != shape.name {
			continue
		}
		if candidate.Status != shape.state || candidate.ConstraintNoValidate != shape.noValidate {
			if shape.name != "" {
				return true, false, false, nil
			}
			continue
		}
		if shape.index != "" && candidate.ConstraintIndex != shape.index {
			if shape.name != "" {
				return true, false, false, nil
			}
			continue
		}
		var ddl sql.NullString
		if err = w.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('CONSTRAINT',?,?)`, candidate.Name, w.schema).Scan(&ddl); err != nil {
			return true, false, false, err
		}
		actualTokens, e := nativeSQLTokens(ddl.String)
		if e != nil {
			return true, false, false, e
		}
		actual, valid := nativeOwnedConstraintShape(actualTokens, w.schema, object.Name)
		if !ddl.Valid || !valid || !nativeDefinitionsEqual(shape.body, actual.body) {
			if shape.name != "" {
				return true, false, false, nil
			}
			continue
		}
		matched++
	}
	if shape.name != "" {
		return true, matched == 1, matched == 0 && !nativeOwnedNamedConstraintExists(snapshot, shape.name), nil
	}
	// Generated names differ across databases. Count equivalent frozen units
	// through this ordinal, so two identical unnamed constraints are not both
	// marked complete by the presence of a single target constraint.
	needed := 0
	for i := 0; i <= unit; i++ {
		previous, e := nativeSQLTokens(object.Units[i].SQL)
		if e != nil {
			return true, false, false, e
		}
		other, valid := nativeOwnedConstraintShape(previous, w.schema, object.Name)
		if valid && other.name == "" && other.kind == shape.kind && other.state == shape.state && other.noValidate == shape.noValidate && other.index == shape.index && nativeDefinitionsEqual(other.body, shape.body) {
			needed++
		}
	}
	return true, matched >= needed, matched < needed, nil
}

func nativeOwnedNamedConstraintExists(snapshot *damengnative.Snapshot, name string) bool {
	for _, object := range snapshot.Objects {
		if object.Kind == "constraint" && object.Name == name {
			return true
		}
	}
	return false
}

func nativeOwnedIndexHead(tokens []string, schema, table string) (string, bool) {
	if len(tokens) < 6 || !strings.EqualFold(tokens[0], "CREATE") {
		return "", false
	}
	i := 1
	if len(tokens) > 3 && strings.EqualFold(tokens[1], "OR") && strings.EqualFold(tokens[2], "REPLACE") {
		i = 3
	}
	for i < len(tokens) && (strings.EqualFold(tokens[i], "UNIQUE") || strings.EqualFold(tokens[i], "CLUSTER") || strings.EqualFold(tokens[i], "BITMAP") || strings.EqualFold(tokens[i], "NOT") || strings.EqualFold(tokens[i], "PARTIAL")) {
		i++
	}
	if i >= len(tokens) || !strings.EqualFold(tokens[i], "INDEX") {
		return "", false
	}
	end, err := nativeHeadIdentity(tokens, i+1, schema)
	if err != nil || end >= len(tokens) || !strings.EqualFold(tokens[end], "ON") {
		return "", false
	}
	name := nativeTokenName(tokens[end-1])
	tableEnd, err := nativeHeadIdentity(tokens, end+1, schema)
	return name, err == nil && tableEnd < len(tokens) && tokens[tableEnd] == "(" && nativeTokenName(tokens[tableEnd-1]) == table
}

type nativeOwnedConstraint struct {
	name, kind, body, state, index string
	noValidate                     bool
}

func nativeOwnedConstraintShape(tokens []string, schema, table string) (nativeOwnedConstraint, bool) {
	var out nativeOwnedConstraint
	if len(tokens) < 5 || !strings.EqualFold(tokens[0], "ALTER") || !strings.EqualFold(tokens[1], "TABLE") {
		return out, false
	}
	end, err := nativeHeadIdentity(tokens, 2, schema)
	if err != nil || end >= len(tokens) || nativeTokenName(tokens[end-1]) != table || !strings.EqualFold(tokens[end], "ADD") {
		return out, false
	}
	i := end + 1
	if i < len(tokens) && strings.EqualFold(tokens[i], "CONSTRAINT") {
		i++
		// DM8 may emit an empty generated name as ADD CONSTRAINT CHECK.
		if i < len(tokens) && !nativeOwnedConstraintStart(tokens, i) {
			out.name = nativeTokenName(tokens[i])
			i++
		}
	}
	if !nativeOwnedConstraintStart(tokens, i) {
		return out, false
	}
	switch strings.ToUpper(tokens[i]) {
	case "CHECK":
		out.kind = "C"
	case "FOREIGN":
		out.kind = "R"
	case "UNIQUE":
		out.kind = "U"
	default:
		out.kind = "P"
	}
	out.state = "ENABLED"
	var body []string
	depth := 0
	for ; i < len(tokens); i++ {
		t := tokens[i]
		if t == "(" {
			depth++
		} else if t == ")" {
			depth--
		}
		if depth == 0 {
			switch strings.ToUpper(t) {
			case "ENABLE", "VALIDATE", ";":
				continue
			case "DISABLE":
				out.state = "DISABLED"
				continue
			case "NOVALIDATE":
				out.noValidate = true
				continue
			case "USING":
				if i+2 < len(tokens) && strings.EqualFold(tokens[i+1], "INDEX") {
					j, e := nativeHeadIdentity(tokens, i+2, schema)
					if e != nil {
						return out, false
					}
					out.index = nativeTokenName(tokens[j-1])
					i = j - 1
					continue
				}
			}
		}
		body = append(body, t)
	}
	if out.state == "DISABLED" {
		out.noValidate = false
	}
	out.body = fmt.Sprintf("ALTER TABLE %s ADD %s", damengnative.Qualified(schema, table), strings.Join(body, " "))
	return out, depth == 0
}

func nativeOwnedConstraintStart(tokens []string, i int) bool {
	if i >= len(tokens) {
		return false
	}
	if strings.HasPrefix(tokens[i], `"`) {
		return false
	}
	switch strings.ToUpper(tokens[i]) {
	case "CHECK", "UNIQUE", "PRIMARY", "FOREIGN", "CLUSTER", "NOT":
		return true
	}
	return false
}
