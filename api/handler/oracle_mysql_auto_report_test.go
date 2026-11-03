package handler

import (
	"encoding/json"
	"testing"

	"dbgold/datamigrate"
	"github.com/stretchr/testify/require"
)

func TestOracleMySQLHistoricalTriggerReportDoesNotInferSuccess(t *testing.T) {
	body, err := enrichOracleMySQLTriggerReport([]byte(`{"sequences":{"total":2,"success":2},"extra":"preserved"}`))
	require.NoError(t, err)
	var report datamigrate.MigrationReport
	require.NoError(t, json.Unmarshal(body, &report))
	require.NotNil(t, report.TriggerAdaptation)
	require.False(t, report.TriggerAdaptation.Recorded)
	require.Zero(t, report.TriggerAdaptation.Success)
	require.Contains(t, string(body), `"extra":"preserved"`)
}
func TestOracleMySQLRecordedTriggerReportRemainsFrozen(t *testing.T) {
	original := []byte(`{"trigger_adaptation":{"mode":"mysql_auto_increment","recorded":true,"success":1,"items":[{"start":"9007199254740994"}]}}`)
	body, err := enrichOracleMySQLTriggerReport(original)
	require.NoError(t, err)
	require.Equal(t, original, body)
}
