package damengnative

import "strings"

func declaredNativeType(table Object, column, owner string, objects []Object) (string, string, bool) {
	for _, u := range table.Units {
		if u.Phase != "before_data" {
			continue
		}
		ts, err := nativeTokens(u.SQL)
		if err != nil {
			continue
		}
		open := -1
		for i, t := range ts {
			if t.value == "(" {
				open = i
				break
			}
		}
		if open < 0 {
			continue
		}
		depth := 1
		start := true
		for i := open + 1; i < len(ts) && depth > 0; i++ {
			t := ts[i]
			if t.value == "(" {
				depth++
				continue
			}
			if t.value == ")" {
				depth--
				continue
			}
			if depth != 1 {
				continue
			}
			if t.value == "," {
				start = true
				continue
			}
			if !start {
				continue
			}
			start = false
			matches := t.kind == 'q' && t.value == column || t.kind == 'i' && strings.ToUpper(t.value) == column
			if !matches || i+1 >= len(ts) {
				continue
			}
			first := ts[i+1]
			if first.kind != 'q' && first.kind != 'i' {
				return "", "", false
			}
			schema, name := owner, first.value
			if first.kind == 'i' {
				name = strings.ToUpper(name)
			}
			qualified := false
			if i+3 < len(ts) && ts[i+2].value == "." {
				qualified = true
				schema, name = name, ts[i+3].value
				if ts[i+3].kind == 'i' {
					name = strings.ToUpper(name)
				}
			}
			for _, o := range objects {
				if o.Schema == schema && o.Name == name {
					if o.Kind == "domain" {
						return "", "", false
					}
					if o.Kind == "type" || o.Kind == "class" {
						return schema, name, true
					}
				}
			}
			// Qualified user-defined/spatial types in another schema are still
			// native UDTs; a missing target type is caught by dependency preflight.
			if qualified && schema != "SYS" {
				return schema, name, true
			}
			return "", "", false
		}
	}
	return "", "", false
}
