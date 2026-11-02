package handler

import (
	"encoding/json"

	"dbgold/datamigrate/source"
)

// Historical reports can recover source types only from their own frozen source
// columns. Enrichment affects the response, preserves unknown fields, and never
// consults the current source/target schema or mutable repair ledger.
func enrichAutoRepairSourceTypes(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if len(fields["auto_repairs"]) == 0 || len(fields["repair_manifest"]) == 0 {
		return body, nil
	}
	var manifest struct {
		Objects []struct {
			Kind          string              `json:"kind"`
			Name          string              `json:"name"`
			SourceName    string              `json:"source_name"`
			SourceTable   string              `json:"source_table"`
			SourceColumns []source.ColumnInfo `json:"source_columns"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(fields["repair_manifest"], &manifest); err != nil {
		return nil, err
	}
	types := make(map[string]map[string]string)
	for _, object := range manifest.Objects {
		if object.Kind != "table" || len(object.SourceColumns) == 0 {
			continue
		}
		table := object.SourceTable
		if table == "" {
			table = object.SourceName
		}
		if table == "" {
			table = object.Name
		}
		if types[table] == nil {
			types[table] = make(map[string]string)
		}
		for _, col := range object.SourceColumns {
			types[table][col.Name] = source.FormatColumnType(col)
		}
	}
	var repairs []map[string]json.RawMessage
	if err := json.Unmarshal(fields["auto_repairs"], &repairs); err != nil {
		return nil, err
	}
	changed := false
	for _, item := range repairs {
		var existing, table, column string
		if err := json.Unmarshal(item["source_type"], &existing); err == nil && existing != "" {
			continue
		}
		if json.Unmarshal(item["table"], &table) != nil || json.Unmarshal(item["source"], &column) != nil {
			continue
		}
		if typ := types[table][column]; typ != "" {
			item["source_type"], _ = json.Marshal(typ)
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	var err error
	fields["auto_repairs"], err = json.Marshal(repairs)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
