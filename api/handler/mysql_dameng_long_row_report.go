package handler

import (
	"bytes"
	"cmp"
	"encoding/json"

	"dbgold/datamigrate"
)

// Older reports have the original driver error but no suggestion. Enrich
// only the returned data items, using target names frozen with the job.
func enrichMySQLDamengLongRowReport(body []byte, schema string) ([]byte, error) {
	if !bytes.Contains(body, []byte("-2665")) {
		return body, nil
	}
	var report, data map[string]json.RawMessage
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	if !bytes.Contains(report["data"], []byte("-2665")) {
		return body, nil
	}
	if err := json.Unmarshal(report["data"], &data); err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if len(data["items"]) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(data["items"], &items); err != nil {
		return nil, err
	}
	type tableTarget struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		SourceName  string `json:"source_name"`
		SourceTable string `json:"source_table"`
		TargetName  string `json:"target_name"`
		Schema      string `json:"schema"`
	}
	var manifest struct {
		Config  struct{ TargetSchema string } `json:"config"`
		Objects []tableTarget                 `json:"objects"`
	}
	targets := make(map[string]tableTarget)
	if raw := report["repair_manifest"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return nil, err
		}
		schema = cmp.Or(manifest.Config.TargetSchema, schema)
		for _, obj := range manifest.Objects {
			if source := cmp.Or(obj.SourceTable, obj.SourceName, obj.Name); obj.Kind == "table" && source != "" {
				targets[source] = obj
			}
		}
	}
	changed := false
	for i, raw := range items {
		var item datamigrate.ObjectResult
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		target := targets[cmp.Or(item.SourceName, item.Table, item.Name)]
		if !item.SuggestMySQLDamengLongRow(cmp.Or(target.Schema, schema), target.TargetName, false) {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		fields["suggestion"], _ = json.Marshal(item.Suggestion)
		if item.SuggestedDDL != "" {
			fields["suggested_ddl"], _ = json.Marshal(item.SuggestedDDL)
		}
		items[i], _ = json.Marshal(fields)
		changed = true
	}
	if !changed {
		return body, nil
	}
	data["items"], _ = json.Marshal(items)
	report["data"], _ = json.Marshal(data)
	return json.Marshal(report)
}
