package handler

import "dbgold/datamigrate/repair"

// recoveryPreviewObject reloads sibling data records before each preview. The
// derived state belongs only to the preview copy, never to the saved draft.
func recoveryPreviewObject(jobID string, object repair.Object) (repair.Object, error) {
	objects, _, err := repairObjects(jobID)
	if err != nil {
		return object, err
	}
	return recoveryObjectWithDataState(object, objects), nil
}

// recoveryObjectWithDataState follows the display grouping rules so a successful
// table record cannot skip data that an associated data record still needs.
func recoveryObjectWithDataState(object repair.Object, objects []repair.Object) repair.Object {
	if !recoveryStateTableKind(object.Kind) || object.DiagnosticOnly || !recoveryStateCommitted(object.DataStatus) {
		return object
	}
	byID := make(map[string]repair.Object, len(objects)+1)
	unique := make([]repair.Object, 0, len(objects)+1)
	for _, item := range objects {
		if _, exists := byID[item.ID]; exists {
			continue
		}
		byID[item.ID] = item
		unique = append(unique, item)
	}
	if _, exists := byID[object.ID]; !exists {
		byID[object.ID] = object
		unique = append(unique, object)
	}
	tables := make([]repair.Object, 0, len(unique))
	for _, item := range unique {
		if recoveryStateTableKind(item.Kind) {
			tables = append(tables, item)
		}
	}
	sources := map[string]bool{}
	if name, _ := recoveryStateIdentity(byID[object.ID], true); name != "" {
		sources[name] = true
	}
	unfinished := false
	for _, data := range unique {
		if data.Kind != "data" || data.DiagnosticOnly {
			continue
		}
		dependencies := map[string]bool{}
		resolved := true
		for _, id := range data.Dependencies {
			dependencies[id] = true
			if _, exists := byID[id]; !exists {
				resolved = false
			}
		}
		if !resolved {
			continue
		}
		var candidates []repair.Object
		if len(dependencies) > 0 {
			for _, table := range tables {
				if dependencies[table.ID] {
					candidates = append(candidates, table)
				}
			}
		} else {
			name, valid := recoveryStateIdentity(data, false)
			if !valid || name == "" {
				continue
			}
			for _, table := range tables {
				targetName, _ := recoveryStateIdentity(table, false)
				if !table.DiagnosticOnly && table.Schema == data.Schema && targetName == name {
					candidates = append(candidates, table)
				}
			}
		}
		if len(candidates) != 1 || candidates[0].ID != object.ID || candidates[0].DiagnosticOnly || !recoveryStateCompatibleTable(candidates[0], data) {
			continue
		}
		if name, _ := recoveryStateIdentity(data, true); name != "" {
			sources[name] = true
		}
		status := data.DataStatus
		if status == "" {
			status = data.Status
		}
		unfinished = unfinished || !recoveryStateCommitted(status) && status != "not_required"
	}
	// A table without source metadata must not claim data from different sources.
	if len(sources) <= 1 && unfinished {
		object.DataStatus = "unknown"
	}
	return object
}

func recoveryStateTableKind(kind string) bool {
	return kind == "table" || kind == "tables"
}

func recoveryStateCommitted(status string) bool {
	return status == "succeeded" || status == "success" || status == "repaired"
}

func recoveryStateIdentity(object repair.Object, source bool) (string, bool) {
	first, second := object.Table, object.TargetName
	if source {
		first, second = object.SourceTable, object.SourceName
	}
	if first == "" {
		return second, true
	}
	return first, second == "" || first == second
}

func recoveryStateCompatibleTable(table, data repair.Object) bool {
	if table.Schema != data.Schema {
		return false
	}
	for _, source := range []bool{false, true} {
		tableName, tableValid := recoveryStateIdentity(table, source)
		dataName, dataValid := recoveryStateIdentity(data, source)
		if !tableValid || !dataValid || tableName != "" && dataName != "" && tableName != dataName {
			return false
		}
	}
	return true
}
