package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func migrationTriggerReportRequest(jobID string, owner uint, role string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/jobs/:jobID/report", func(c *gin.Context) {
		c.Set("userID", owner)
		c.Set("role", role)
		GetDataMigrationReport(c)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/jobs/"+jobID+"/report", nil))
	return recorder
}

func migrationTriggerReportJob(t *testing.T, src, dst, body string) {
	t.Helper()
	migrationHandlerTestDB(t)
	require.NoError(t, store.DB.AutoMigrate(&store.DataMigrationReport{}))
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "trigger-report", OwnerID: 7, SrcDBType: src, DstDBType: dst, Status: "failed"}))
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: "trigger-report", ReportJSON: body}))
}

func TestGetDataMigrationReportTriggerAdaptationEnforcesOwnershipAndAdmin(t *testing.T) {
	migrationTriggerReportJob(t, "oracle", "dameng", `{ "future_field": true }`)
	for _, tc := range []struct {
		name  string
		owner uint
		role  string
		code  int
	}{
		{"owner", 7, "user", http.StatusOK},
		{"other owner", 8, "user", http.StatusNotFound},
		{"admin", 99, "admin", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := migrationTriggerReportRequest("trigger-report", tc.owner, tc.role)
			require.Equal(t, tc.code, response.Code, response.Body.String())
			if tc.code == http.StatusOK {
				var report struct {
					Adaptation struct{ Recorded bool } `json:"trigger_adaptation"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
				require.False(t, report.Adaptation.Recorded)
			}
		})
	}
	require.Equal(t, http.StatusNotFound, migrationTriggerReportRequest("missing", 7, "user").Code)
	require.NoError(t, store.DB.Delete(&store.DataMigrationReport{}, "job_id = ?", "trigger-report").Error)
	require.Equal(t, http.StatusNotFound, migrationTriggerReportRequest("trigger-report", 7, "user").Code)
}

func frozenOracleDamengTriggerReport() datamigrate.MigrationReport {
	report := datamigrate.MigrationReport{
		Sequences:      datamigrate.CategoryReport{Total: 62, Success: 58, Failed: 4},
		Triggers:       datamigrate.CategoryReport{Total: 9, Failed: 9, Items: []datamigrate.ObjectResult{{Name: "historic source trigger", Error: "original report evidence"}}},
		RepairManifest: &datamigrate.RepairManifest{Version: 1, Objects: []repair.Object{}},
	}
	for i := 0; i < 62; i++ {
		seq := source.SequenceInfo{TableName: fmt.Sprintf("TABLE_%d", i), ColumnName: "ID", StartValueText: "1",
			Native: &source.NativeSequenceInfo{Name: fmt.Sprintf("SEQ_%d", i), IncrementBy: "1", MinValue: "1", MaxValue: "1000", CacheSize: "0",
				Bindings: []source.SequenceBinding{{TableName: fmt.Sprintf("TABLE_%d", i), ColumnName: "ID", Kind: "trigger", TriggerName: fmt.Sprintf("TRG_%d", i), OnlyWhenNull: true}},
			},
		}
		object := repair.Object{ID: fmt.Sprintf("sequence-%d", i), Kind: "sequence", Schema: "APP", Table: seq.TableName, TargetName: seq.Native.Name, Status: "succeeded", Sequence: &seq}
		if i >= 58 {
			seq.Unsupported, seq.FailureStage = "source sequence metadata is not visible", "metadata"
			object.Status, object.Error = "failed", seq.Unsupported
		} else {
			for j, statement := range dialect.NewDaMeng().SequenceStatements("APP", seq, "") {
				object.Units = append(object.Units, repair.Unit{ID: fmt.Sprintf("step-%d", j), SQL: statement.SQL, OriginalSQL: statement.SQL, Status: "succeeded"})
			}
		}
		report.RepairManifest.Objects = append(report.RepairManifest.Objects, object)
	}
	return report
}

func TestGetDataMigrationReportTriggerAdaptationUsesFrozenManifestWithoutReadsOrWrites(t *testing.T) {
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(repairJSON(frozenOracleDamengTriggerReport())), &fields))
	fields["future_field"] = json.RawMessage(`{"precise":123456789012345678901234567890,"nested":[true,"untouched"]}`)
	fields["trigger_adaptation"] = json.RawMessage(`{"recorded":true,"total":99999,"unknown":"untrusted cached summary"}`)
	body, err := json.MarshalIndent(fields, "", "  ")
	require.NoError(t, err)
	migrationTriggerReportJob(t, "oracle", "dameng", string(body))
	require.NoError(t, store.DB.AutoMigrate(&store.MigrationRepairObject{}, &store.MigrationRepairBinding{}, &store.MigrationRepairAttempt{}))
	live := store.MigrationRepairObject{JobID: "trigger-report", ObjectID: "sequence-61", Version: 17,
		ObjectJSON: repairJSON(repair.Object{Kind: "sequence", Status: "succeeded", Units: []repair.Unit{{Status: "succeeded"}}})}
	require.NoError(t, store.DB.Create(&live).Error)
	var queries []string
	writes := 0
	require.NoError(t, store.DB.Callback().Query().Before("gorm:query").Register("trigger_report_read_spy", func(db *gorm.DB) {
		queries = append(queries, db.Statement.Table)
	}))
	for _, processor := range []struct {
		register func(string, func(*gorm.DB)) error
		remove   func(string) error
		name     string
	}{
		{store.DB.Callback().Create().Before("gorm:create").Register, store.DB.Callback().Create().Remove, "trigger_report_create_spy"},
		{store.DB.Callback().Update().Before("gorm:update").Register, store.DB.Callback().Update().Remove, "trigger_report_update_spy"},
		{store.DB.Callback().Delete().Before("gorm:delete").Register, store.DB.Callback().Delete().Remove, "trigger_report_delete_spy"},
	} {
		require.NoError(t, processor.register(processor.name, func(*gorm.DB) { writes++ }))
		t.Cleanup(func() { _ = processor.remove(processor.name) })
	}
	t.Cleanup(func() { _ = store.DB.Callback().Query().Remove("trigger_report_read_spy") })
	check := func() {
		queries, writes = nil, 0
		response := migrationTriggerReportRequest("trigger-report", 7, "user")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, []string{"data_migration_jobs", "data_migration_reports"}, queries, "GET must not query current repair records, bindings, attempts or connections")
		require.Zero(t, writes, "GET must not seed or update repair records or the stored report")
		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
		require.JSONEq(t, `{"recorded":true,"total":62,"success":58,"failed":0,"blocked":4,"pending":0,"unknown":0}`, string(got["trigger_adaptation"]))
		for key, original := range fields {
			if key != "trigger_adaptation" {
				require.JSONEq(t, string(original), string(got[key]), "historic report field %s must remain intact", key)
			}
		}
	}
	check()
	changedLedger := repairJSON(repair.Object{Kind: "sequence", Status: "unknown", Units: []repair.Unit{{Status: "unknown"}}})
	require.NoError(t, store.DB.Model(&live).Update("object_json", changedLedger).Error)
	check()
	stored, err := store.GetDataMigrationReport("trigger-report")
	require.NoError(t, err)
	require.Equal(t, string(body), stored.ReportJSON, "summary enrichment must never rewrite historical JSON")
	var after store.MigrationRepairObject
	require.NoError(t, store.DB.First(&after, live.ID).Error)
	require.Equal(t, 17, after.Version)
	require.Equal(t, changedLedger, after.ObjectJSON)
}

func TestGetDataMigrationReportTriggerAdaptationLegacyAndNullAreUnrecorded(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"future_field":{"keep":true},"trigger_adaptation":{"recorded":true,"success":999}}`,
		`{"repair_manifest":null,"sequences":{"total":62,"success":58,"failed":4}}`,
		`{"repair_manifest":{"version":99,"objects":[]},"sequences":{"total":1}}`,
		`{"repair_manifest":{"version":1,"objects":[]},"sequences":{"total":1}}`,
	} {
		t.Run(body, func(t *testing.T) {
			migrationTriggerReportJob(t, "oracle", "dameng", body)
			response := migrationTriggerReportRequest("trigger-report", 7, "user")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var report struct {
				Summary struct {
					Recorded bool `json:"recorded"`
					Total    int  `json:"total"`
				} `json:"trigger_adaptation"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
			require.False(t, report.Summary.Recorded)
			require.Zero(t, report.Summary.Total)
			stored, err := store.GetDataMigrationReport("trigger-report")
			require.NoError(t, err)
			require.Equal(t, body, stored.ReportJSON)
		})
	}
}

func TestGetDataMigrationReportOtherPairsKeepExactRawResponse(t *testing.T) {
	for _, pair := range [][2]string{{"mysql", "dameng"}, {"oracle", "postgres"}, {"postgres", "mysql"}} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			body := " {\n  \"unknown\": 123456789012345678901234567890, \"trigger_adaptation\": {\"recorded\":true,\"total\":55}\n}\n"
			migrationTriggerReportJob(t, pair[0], pair[1], body)
			response := migrationTriggerReportRequest("trigger-report", 7, "user")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, body, response.Body.String())
		})
	}
}

func TestGetDataMigrationReportTriggerAdaptationMalformedReportReturnsError(t *testing.T) {
	for _, body := range []string{`{`, `[]`, `{"repair_manifest":"invalid"}`, `{"sequences":"invalid"}`} {
		t.Run(body, func(t *testing.T) {
			migrationTriggerReportJob(t, "oracle", "dameng", body)
			response := migrationTriggerReportRequest("trigger-report", 7, "user")
			require.Equal(t, http.StatusInternalServerError, response.Code)
			require.JSONEq(t, `{"error":"报告格式错误"}`, response.Body.String())
			stored, err := store.GetDataMigrationReport("trigger-report")
			require.NoError(t, err)
			require.Equal(t, body, stored.ReportJSON)
		})
	}
}
