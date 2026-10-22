package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

type nativeTriggerPlanTestReader struct{ *triggerPlanTestReader }

func (*nativeTriggerPlanTestReader) GetTriggerCharacterMetadata(context.Context) (*source.TriggerCharacterMetadata, error) {
	return &source.TriggerCharacterMetadata{
		ServerVersion:  "8.0.42 Source distribution",
		NumericColumns: []source.TriggerNumericColumn{{Schema: "Source", Table: "Orders", Name: "Value", ColumnType: "int"}},
	}, nil
}

type nativeTriggerPlanTestWriter struct {
	triggerPlanTestWriter
	supported bool
}

func (*nativeTriggerPlanTestWriter) DBType() string { return "kingbase" }
func (*nativeTriggerPlanTestWriter) Dialect() dialect.Dialect {
	return dialect.NewPostgres("kingbase")
}
func (w *nativeTriggerPlanTestWriter) DetectMySQLPGNativeUserVariables(context.Context) (bool, error) {
	return w.supported, nil
}

func seedNativeTriggerPlanHandler(t *testing.T) (repair.Object, *nativeTriggerPlanTestReader, *nativeTriggerPlanTestWriter) {
	t.Helper()
	object, baseReader := seedTriggerPlanHandler(t)
	binding, err := store.GetMigrationRepairBinding("job")
	require.NoError(t, err)
	dst, err := store.GetConnection(binding.DstConnID)
	require.NoError(t, err)
	dst.DBType = "kingbase"
	require.NoError(t, store.DB.Model(&store.Connection{}).Where("id = ?", dst.ID).Update("db_type", dst.DBType).Error)
	var cfg datamigrate.Config
	require.NoError(t, json.Unmarshal([]byte(binding.ConfigJSON), &cfg))
	cfg.TargetDBType = dst.DBType
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Updates(map[string]any{"dst_db_type": dst.DBType, "repair_config_json": repairJSON(cfg)}).Error)
	require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Updates(map[string]any{"target_fingerprint": connectionFingerprint(dst), "config_json": repairJSON(cfg)}).Error)

	baseReader.trigger.Name = "BeforeUpdateSessionValue"
	baseReader.trigger.Event = "UPDATE"
	baseReader.trigger.Body = "SET @CurrentValue=OLD.Value+1"
	baseReader.trigger.SourceDDL = "CREATE TRIGGER BeforeUpdateSessionValue BEFORE UPDATE ON Orders FOR EACH ROW SET @CurrentValue=OLD.Value+1"
	object.Name, object.SourceName, object.TargetName = baseReader.trigger.Name, baseReader.trigger.Name, baseReader.trigger.Name
	object.SourceDDL = baseReader.trigger.SourceDDL
	object.Error = "仅支持 SET NEW.列 = 表达式；读取源字符排序元数据失败: 当前 8.0.42 Source distribution"
	object.OriginalError = object.Error
	require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", object.ID).Update("object_json", repairJSON(object)).Error)
	report, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	var frozen datamigrate.MigrationReport
	require.NoError(t, json.Unmarshal([]byte(report.ReportJSON), &frozen))
	frozen.Triggers.Items[0].Name = object.Name
	frozen.Triggers.Items[0].SourceName = object.SourceName
	frozen.Triggers.Items[0].SourceDDL = object.SourceDDL
	frozen.Triggers.Items[0].Error = object.Error
	require.NoError(t, store.DB.Model(&store.DataMigrationReport{}).Where("job_id = ?", "job").Update("report_json", repairJSON(frozen)).Error)

	reader := &nativeTriggerPlanTestReader{triggerPlanTestReader: baseReader}
	writer := &nativeTriggerPlanTestWriter{supported: true}
	newMigrationTriggerPlanReader = func(conn *store.Connection, database string, _ source.ConnPoolConfig, targets ...string) (source.Reader, error) {
		require.Equal(t, "mysql", conn.DBType)
		require.Equal(t, "Source", database)
		require.Equal(t, []string{"kingbase"}, targets)
		return reader, nil
	}
	newMigrationTriggerPlanWriter = func(conn *store.Connection, schema string, _ target.ConnPoolConfig) (target.Writer, error) {
		require.Equal(t, "kingbase", conn.DBType)
		require.Equal(t, "Target", schema)
		return writer, nil
	}
	return object, reader, writer
}

func TestNativeTriggerPlanPreviewAndSavePreserveOriginalFailure(t *testing.T) {
	original, reader, _ := seedNativeTriggerPlanHandler(t)
	before, err := store.GetMigrationRepairObject("job", "trg")
	require.NoError(t, err)
	report, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)

	preview := previewTriggerPlan(t)
	require.NotNil(t, preview.Object.MySQLPGTrigger)
	plan := preview.Object.MySQLPGTrigger
	require.Equal(t, 4, plan.Version)
	require.True(t, plan.NativeUserVariables)
	require.Contains(t, plan.FunctionBody, "PERFORM @CurrentValue :=")
	require.Contains(t, plan.FunctionBody, `CAST(OLD."Value" AS bigint)`)
	require.NotNil(t, plan.CharacterMetadata)
	require.Equal(t, "8.0.42 Source distribution", plan.CharacterMetadata.ServerVersion)
	require.NoError(t, mysqltrigger.ValidatePlan(*plan))
	require.Len(t, preview.Object.Units, 2)
	require.Equal(t, original.SourceDDL, preview.Object.SourceDDL)
	require.Equal(t, original.OriginalError, preview.Object.OriginalError)

	after, err := store.GetMigrationRepairObject("job", "trg")
	require.NoError(t, err)
	require.Equal(t, before.ObjectJSON, after.ObjectJSON)
	require.Equal(t, before.Version, after.Version)
	afterReport, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	require.Equal(t, report.ReportJSON, afterReport.ReportJSON)

	response := repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", saveTriggerPlanBody(preview), 1, SaveMigrationTriggerPlan)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var saved triggerPlanTestResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &saved))
	require.Equal(t, 2, saved.Version)
	require.Equal(t, 2, reader.reads)
	require.NotNil(t, saved.Object.MySQLPGTrigger)
	require.Equal(t, *plan, *saved.Object.MySQLPGTrigger)
	require.True(t, saved.Object.MySQLPGTrigger.NativeUserVariables)
	require.Len(t, saved.Object.Units, 2)
	for _, unit := range saved.Object.Units {
		require.True(t, unit.Selected)
		require.Equal(t, "pending", unit.Status)
		require.Equal(t, unit.SQL, unit.OriginalSQL)
	}
	require.Equal(t, original.SourceDDL, saved.Object.SourceDDL)
	require.Equal(t, original.OriginalError, saved.Object.OriginalError)
	require.Equal(t, original.OriginalStatus, saved.Object.OriginalStatus)
	require.NoError(t, validateRepairObjectScope(saved.Object, saved.Object.Units, "kingbase"))
	persisted, err := store.GetMigrationRepairObject("job", "trg")
	require.NoError(t, err)
	var persistedObject repair.Object
	require.NoError(t, json.Unmarshal([]byte(persisted.ObjectJSON), &persistedObject))
	require.Equal(t, saved.Object.MySQLPGTrigger, persistedObject.MySQLPGTrigger)
	afterReport, err = store.GetDataMigrationReport("job")
	require.NoError(t, err)
	require.Equal(t, report.ReportJSON, afterReport.ReportJSON)
	var attempts int64
	require.NoError(t, store.DB.Model(&store.MigrationRepairAttempt{}).Count(&attempts).Error)
	require.Zero(t, attempts)
}

func TestNativeTriggerPlanSaveRejectsChangedSourceOrCapability(t *testing.T) {
	for _, change := range []string{"source", "target mode"} {
		t.Run(change, func(t *testing.T) {
			_, reader, writer := seedNativeTriggerPlanHandler(t)
			preview := previewTriggerPlan(t)
			before, err := store.GetMigrationRepairObject("job", "trg")
			require.NoError(t, err)
			if change == "source" {
				reader.trigger.SourceDDL += " -- changed"
			} else {
				writer.supported = false
			}
			response := repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", saveTriggerPlanBody(preview), 1, SaveMigrationTriggerPlan)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			after, err := store.GetMigrationRepairObject("job", "trg")
			require.NoError(t, err)
			require.Equal(t, before.ObjectJSON, after.ObjectJSON)
			require.Equal(t, before.Version, after.Version)
		})
	}
}
