package datamigrate

import "dbgold/datamigrate/damengnative"

// User-named constraints and their indexes retain schema-wide names even when
// their table creates them. They may be replaced with that table, but never by
// silently taking a name owned by a different, retained target table.
func nativeNamedOwnedObject(object damengnative.Object) bool {
	if object.Generated {
		return false
	}
	return nativeOwnedObjectKind(object)
}

func nativeOwnedObjectKind(object damengnative.Object) bool {
	switch object.Kind {
	case "constraint", "primary_key", "foreign_key", "unique", "check", "index", "context_index":
		return true
	}
	return false
}

func nativeOwnedNameConflicts(planned, actual []damengnative.Object, drops map[string]bool) ([]damengnative.Object, []damengnative.Issue) {
	byName := map[string]damengnative.Object{}
	byID := map[string]damengnative.Object{}
	tables := map[string]string{}
	for _, object := range actual {
		byID[object.ID] = object
		if object.Kind == "table" {
			tables[object.Name] = object.ID
		}
		// A generated target name still occupies its namespace. A frozen
		// user-specified source name cannot take it from a retained table.
		if nativeOwnedObjectKind(object) {
			byName[nativeCollisionKey(object)] = object
		}
	}
	ownerReplaced := func(object damengnative.Object) bool {
		seen := map[string]bool{}
		parent := object.ParentID
		if parent == "" {
			parent = tables[object.Table]
		}
		for parent != "" && !seen[parent] {
			if drops[parent] {
				return true
			}
			seen[parent] = true
			parent = byID[parent].ParentID
		}
		return false
	}
	var conflicts []damengnative.Object
	var issues []damengnative.Issue
	seenNames := map[string]bool{}
	for _, object := range planned {
		if !nativeNamedOwnedObject(object) {
			continue
		}
		key := nativeCollisionKey(object)
		if seenNames[key] {
			continue
		}
		seenNames[key] = true
		collision, exists := byName[key]
		if !exists || !object.Managed && !collision.Managed || ownerReplaced(collision) {
			continue
		}
		conflicts = append(conflicts, collision)
		issues = append(issues, damengnative.Issue{ObjectID: object.ID, Kind: object.Kind, Name: object.Name, Severity: "error", Reason: "同名约束或索引属于不会删除的目标对象，不能在保留其所属对象的同时覆盖该名称"})
	}
	return conflicts, issues
}
