package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengLongRowReport(t *testing.T) {
	const body = `{"future":12345678901234567890,"data":{"future_data":true,"items":[
		{"name":"cg_mixed","error":"表 cg_mixed 第 1 页写入失败: 表 cg_mixed 页内第 41 行: Error -2665: 记录超长","future_item":42},
		{"name":"Another","error":"表 Another 页内第 1–41 行: Error 121: batch error\nrow[40]:-2665, record too long"},
		{"name":"Other","error":"Error -26650: another error"},
		{"name":"Check","stage":"preflight","error":"Error -2665: preflight"},
		{"name":"Current","error":"Error -2665: recorded","suggestion":"already diagnosed"},
		{"name":"Batch","target_schema":"Explicit","target_table":"My\"Table","error":"Error 6093: batch error\nrow[2]:Error -2665: record too long"}]},
		"repair_manifest":{"config":{"TargetSchema":"DefaultSchema"},"objects":[
			{"kind":"table","source_table":"cg_mixed","target_name":"Mapped","schema":"SavedSchema"}]}}`
	for _, src := range []string{"mysql", "sqlserver"} {
		t.Run(src, func(t *testing.T) {
			migrationTriggerReportJob(t, src, "dameng", body)
			res := migrationTriggerReportRequest("trigger-report", 7, "user")
			require.Equal(t, http.StatusOK, res.Code)
			var report datamigrate.MigrationReport
			require.NoError(t, json.Unmarshal(res.Body.Bytes(), &report))
			items := report.Data.Items
			require.Len(t, items, 6)
			if src == "mysql" {
				require.Equal(t, `ALTER TABLE "SAVEDSCHEMA"."MAPPED" ENABLE USING LONG ROW;`, items[0].SuggestedDDL)
				require.Contains(t, items[0].Error, "页内第 41 行")
				require.Equal(t, `ALTER TABLE "DEFAULTSCHEMA"."ANOTHER" ENABLE USING LONG ROW;`, items[1].SuggestedDDL)
				require.Empty(t, items[2].Suggestion)
				require.Empty(t, items[3].Suggestion)
				require.Equal(t, "already diagnosed", items[4].Suggestion)
				require.Empty(t, items[4].SuggestedDDL)
				require.Equal(t, `ALTER TABLE "EXPLICIT"."MY""TABLE" ENABLE USING LONG ROW;`, items[5].SuggestedDDL)
				for _, field := range []string{`"future":12345678901234567890`, `"future_data":true`, `"future_item":42`} {
					require.Contains(t, res.Body.String(), field)
				}
			} else {
				require.JSONEq(t, body, res.Body.String())
			}
			saved, err := store.GetDataMigrationReport("trigger-report")
			require.NoError(t, err)
			require.Equal(t, body, saved.ReportJSON)
		})
	}
}

func TestMySQLDamengLongRowAdviceRequiresKnownTarget(t *testing.T) {
	item := datamigrate.ObjectResult{Name: "T", Error: "Error -2665: record too long"}
	require.True(t, item.SuggestMySQLDamengLongRow("", "", false))
	require.Contains(t, item.Suggestion, "目标模式")
	require.Empty(t, item.SuggestedDDL)
	item.Suggestion = ""
	require.True(t, item.SuggestMySQLDamengLongRow("APP", "T", true))
	require.Contains(t, item.Suggestion, "实际字节数")
	require.Empty(t, item.SuggestedDDL)
}

func TestMySQLDamengLongRowHistoricalTarget(t *testing.T) {
	const body = `{"data":{"items":[{"name":"cg_projectdailichouqu","error":"Error -2665: 记录超长"}]}}`
	for _, schema := range []string{"SavedOwner", ""} {
		updated, err := enrichMySQLDamengLongRowReport([]byte(body), schema)
		require.NoError(t, err)
		var report datamigrate.MigrationReport
		require.NoError(t, json.Unmarshal(updated, &report))
		item := report.Data.Items[0]
		if schema == "" {
			require.Empty(t, item.SuggestedDDL)
			require.Contains(t, item.Suggestion, "目标模式")
		} else {
			require.Equal(t, `ALTER TABLE "SAVEDOWNER"."CG_PROJECTDAILICHOUQU" ENABLE USING LONG ROW;`, item.SuggestedDDL)
		}
	}
}
