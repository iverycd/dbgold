package handler

import (
	"encoding/json"

	"dbgold/datamigrate"
)

// Enrich only the response. Historical report fields and the frozen manifest
// remain intact; current repair records and target databases are never read.
func enrichOracleDamengTriggerReport(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	var report datamigrate.MigrationReport
	if manifest, ok := fields["repair_manifest"]; ok {
		if err := json.Unmarshal(manifest, &report.RepairManifest); err != nil {
			return nil, err
		}
	}
	if sequences, ok := fields["sequences"]; ok {
		if err := json.Unmarshal(sequences, &report.Sequences); err != nil {
			return nil, err
		}
	}
	summary, err := json.Marshal(datamigrate.BuildOracleDamengTriggerAdaptation(report))
	if err != nil {
		return nil, err
	}
	fields["trigger_adaptation"] = summary
	return json.Marshal(fields)
}
