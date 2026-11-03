package handler

import (
	"encoding/json"

	"dbgold/datamigrate"
)

// Old Oracle -> MySQL reports contain no conversion evidence. Do not infer it
// from sequence counts or current target objects.
func enrichOracleMySQLTriggerReport(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	if v, ok := fields["trigger_adaptation"]; ok && string(v) != "null" {
		return body, nil
	}
	v, err := json.Marshal(datamigrate.TriggerAdaptationSummary{Mode: "mysql_auto_increment", Recorded: false})
	if err != nil {
		return nil, err
	}
	fields["trigger_adaptation"] = v
	return json.Marshal(fields)
}

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
