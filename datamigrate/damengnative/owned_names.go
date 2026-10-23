package damengnative

import "strings"

// DeclaredTableChildren includes names created inside frozen table units. In
// particular synthetic snapshot buffers have no source dictionary rows for
// their renamed constraints, but those exact names still participate in target
// conflict checking and ownership protection.
func DeclaredTableChildren(objects []Object) []Object {
	var result []Object
	seen := map[string]bool{}
	for _, object := range objects {
		if object.Kind != "table" {
			continue
		}
		for _, unit := range object.Units {
			tokens, err := nativeTokens(unit.SQL)
			if err != nil {
				continue // The normal frozen-unit validator reports malformed SQL.
			}
			depth := 0
			for i, token := range tokens {
				kind, next := "", i+1
				if depth == 1 && token.kind == 'i' && strings.EqualFold(token.value, "CONSTRAINT") {
					kind = "constraint"
				}
				if token.kind == 'i' && strings.EqualFold(token.value, "CREATE") {
					if next+1 < len(tokens) && strings.EqualFold(tokens[next].value, "OR") && strings.EqualFold(tokens[next+1].value, "REPLACE") {
						next += 2
					}
					for next < len(tokens) && tokens[next].kind == 'i' && declaredIndexModifier(tokens[next].value) {
						next++
					}
					if next < len(tokens) && tokens[next].kind == 'i' && strings.EqualFold(tokens[next].value, "INDEX") {
						kind, next = "index", next+1
					}
				}
				if kind != "" {
					owner, name, ok := declaredChildName(tokens, next, object.Schema)
					key := kind + "\x00" + owner + "\x00" + name
					if ok && owner == object.Schema && !seen[key] {
						seen[key] = true
						result = append(result, Object{ID: ObjectID(owner, kind, name), Kind: kind, Schema: owner, Name: name, Table: object.Name, ParentID: object.ID, Managed: true})
					}
				}
				if token.kind == 'p' && token.value == "(" {
					depth++
				} else if token.kind == 'p' && token.value == ")" {
					depth--
				}
			}
		}
	}
	return result
}

func declaredIndexModifier(token string) bool {
	switch strings.ToUpper(token) {
	case "UNIQUE", "BITMAP", "CLUSTER", "NOT", "PARTIAL":
		return true
	}
	return false
}

func declaredChildName(tokens []sqlToken, i int, schema string) (string, string, bool) {
	identifier := func(t sqlToken) (string, bool) {
		if t.kind == 'q' {
			return t.value, true
		}
		if t.kind == 'i' {
			return strings.ToUpper(t.value), true
		}
		return "", false
	}
	if i >= len(tokens) {
		return "", "", false
	}
	name, ok := identifier(tokens[i])
	if !ok {
		return "", "", false
	}
	if i+2 < len(tokens) && tokens[i+1].kind == 'p' && tokens[i+1].value == "." {
		object, valid := identifier(tokens[i+2])
		return name, object, valid
	}
	return schema, name, true
}
