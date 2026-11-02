package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestGetDataMigrationReportSourceTypesPreserveFrozenReport(t *testing.T) {
	body := `{"auto_repairs":[{"table":"Mixed","source":"URL"}],"repair_manifest":{"objects":[{"kind":"table","source_table":"Mixed","source_columns":[{"Name":"URL","DataType":"VARCHAR2","Length":6000}]}]},"future":{"number":12345678901234567890}}`
	migrationTriggerReportJob(t, "oracle", "mysql", body)
	response := migrationTriggerReportRequest("trigger-report", 8, "user")
	require.Equal(t, http.StatusNotFound, response.Code)
	response = migrationTriggerReportRequest("trigger-report", 7, "user")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"source_type":"VARCHAR2(6000)"`)
	require.Contains(t, response.Body.String(), `12345678901234567890`)
	stored, err := store.GetDataMigrationReport("trigger-report")
	require.NoError(t, err)
	require.Equal(t, body, stored.ReportJSON)
}

func TestEnrichAutoRepairSourceTypesUsesFrozenSourceColumns(t *testing.T) {
	body := []byte(`{"future":{"enabled":true},"auto_repairs":[
		{"table":"Mixed","source":"URL","target":"url LONGTEXT","future_item":42},
		{"table":"Mixed","source":"Saved","source_type":"recorded(12)"},
		{"table":"mixed","source":"URL"},
		{"table":"Mixed","source":"Unknown"}],
		"repair_manifest":{"future_manifest":"keep","objects":[{"kind":"table","source_table":"Mixed","columns":[{"Name":"URL","DataType":"LONGTEXT"}],"source_columns":[{"Name":"URL","DataType":"VARCHAR2","Length":6000},{"Name":"Saved","DataType":"NUMBER","Precision":10}]}]}}`)
	got, err := enrichAutoRepairSourceTypes(body)
	require.NoError(t, err)
	var report map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &report))
	var items []map[string]any
	require.NoError(t, json.Unmarshal(report["auto_repairs"], &items))
	require.Equal(t, "VARCHAR2(6000)", items[0]["source_type"])
	require.Equal(t, float64(42), items[0]["future_item"])
	require.Equal(t, "recorded(12)", items[1]["source_type"])
	require.NotContains(t, items[2], "source_type")
	require.NotContains(t, items[3], "source_type")
	require.JSONEq(t, `{"enabled":true}`, string(report["future"]))
	require.Contains(t, string(report["repair_manifest"]), `"future_manifest":"keep"`)
	require.NotContains(t, string(body), `"source_type":"VARCHAR2(6000)"`)
}

func TestEnrichAutoRepairSourceTypesKeepsUnchangedLegacyResponses(t *testing.T) {
	for _, body := range []string{
		`null`, `{ "future": 1 }`,
		`{"auto_repairs":[{"table":"T","source":"C"}]}`,
		`{"auto_repairs":[{"table":"T","source":"C"}],"repair_manifest":{"objects":[]}}`,
		`{"auto_repairs":[{"table":"T","source":"C","source_type":"varchar(3)"}],"repair_manifest":{"objects":[{"kind":"table","name":"T","source_columns":[{"Name":"C","DataType":"int"}]}]}}`,
	} {
		got, err := enrichAutoRepairSourceTypes([]byte(body))
		require.NoError(t, err)
		require.Equal(t, body, string(got))
	}
}

func TestEnrichAutoRepairSourceTypesCombinesTriggerEnrichment(t *testing.T) {
	body := []byte(`{"auto_repairs":[{"table":"T","source":"ROWID"}],"repair_manifest":{"objects":[{"kind":"table","source_name":"T","source_columns":[{"Name":"ROWID","DataType":"int"}]}]},"future":42}`)
	got, err := enrichAutoRepairSourceTypes(body)
	require.NoError(t, err)
	got, err = enrichOracleDamengTriggerReport(got)
	require.NoError(t, err)
	require.Contains(t, string(got), `"source_type":"int"`)
	require.Contains(t, string(got), `"trigger_adaptation"`)
	require.Contains(t, string(got), `"future":42`)
}
