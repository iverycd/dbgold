package handler

import (
	"context"
	"dbgold/datamigrate/mysqltrigger"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

type triggerPlanTestReader struct {
	source.Reader
	trigger   source.TriggerInfo
	reads     int
	typ       string
	afterRead func()
}

func (*triggerPlanTestReader) DBType() string { return "mysql" }
func (*triggerPlanTestReader) Close() error   { return nil }
func (*triggerPlanTestReader) ListTables(context.Context) ([]string, error) {
	return []string{"Orders"}, nil
}
func (r *triggerPlanTestReader) GetTableDDLInfo(context.Context, string) (*source.TableDDLInfo, error) {
	typ := r.typ
	if typ == "" {
		typ = "int"
	}
	return &source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: typ}}}, nil
}
func (r *triggerPlanTestReader) GetTriggers(ctx context.Context) ([]source.TriggerInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.reads++
	if r.afterRead != nil {
		r.afterRead()
	}
	return []source.TriggerInfo{r.trigger}, nil
}

type triggerPlanTestWriter struct{ target.Writer }

func (*triggerPlanTestWriter) DBType() string           { return "postgres" }
func (*triggerPlanTestWriter) Close() error             { return nil }
func (*triggerPlanTestWriter) Dialect() dialect.Dialect { return dialect.NewPostgres("postgres") }

type triggerPlanTestResponse struct {
	Object         repair.Object `json:"object"`
	Version        int           `json:"version"`
	BindingVersion int           `json:"binding_version"`
	PlanHash       string        `json:"plan_hash"`
}

func seedTriggerPlanHandler(t *testing.T) (repair.Object, *triggerPlanTestReader) {
	t.Helper()
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: ":memory:"}))
	db, err := store.DB.DB()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	src := &store.Connection{OwnerID: 1, Name: "Source", DBType: "mysql", Host: "127.0.0.1", Port: 3306, Database: "Source", Username: "reader"}
	dst := &store.Connection{OwnerID: 1, Name: "Target", DBType: "postgres", Host: "127.0.0.1", Port: 5432, Database: "target", Username: "writer"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	cfg := datamigrate.Config{Content: "both", TargetSchema: "Target", TargetDBType: "postgres", MigrateTriggers: true}
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "job", OwnerID: 1, Status: "failed", SrcDBType: "mysql", DstDBType: "postgres", SrcConnID: src.ID, DstConnID: dst.ID, SourceDatabase: "Source", SourceConnectionDatabase: "Source", RepairConfigJSON: repairJSON(cfg), DstSchema: "Target", SrcConnHost: src.Host, SrcConnPort: src.Port, SrcConnDatabase: src.Database, SrcConnUsername: src.Username, DstConnHost: dst.Host, DstConnPort: dst.Port, DstConnDatabase: dst.Database, DstConnUsername: dst.Username}))
	trigger := source.TriggerInfo{Schema: "Source", Name: "BeforeInsert", TableName: "Orders", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1", SourceDDL: "CREATE TRIGGER BeforeInsert BEFORE INSERT ON Orders FOR EACH ROW SET NEW.Value = NEW.Value + 1", SQLMode: "STRICT_TRANS_TABLES"}
	object := repair.Object{ID: "trg", Kind: "trigger", Name: trigger.Name, SourceName: trigger.Name, SourceTable: "Orders", TargetName: trigger.Name, Table: "Orders", Schema: "Target", SourceDDL: trigger.SourceDDL, DiagnosticOnly: true, Status: "failed", OriginalStatus: "failed", Error: "unsupported previous version", OriginalError: "unsupported previous version", RequiresReview: true, Units: []repair.Unit{}, Dependencies: []string{}}
	table := repair.Object{ID: "table", Kind: "table", Name: "Orders", SourceName: "Orders", SourceTable: "Orders", Table: "Orders", TargetName: "Orders", Schema: "Target", Status: "succeeded", DataStatus: "succeeded", ColumnMapping: map[string]string{"Value": "Value"}, Columns: []source.ColumnInfo{{Name: "Value", DataType: dialect.NewPostgres("postgres").MapType(source.ColumnInfo{Name: "Value", DataType: "int"}, "mysql", dialect.TypeOpt{})}}}
	binding := &store.MigrationRepairBinding{JobID: "job", SrcConnID: src.ID, DstConnID: dst.ID, SourceDatabase: "Source", TargetSchema: "Target", SourceFingerprint: connectionFingerprint(src), TargetFingerprint: connectionFingerprint(dst), Configured: true, ConfigJSON: repairJSON(cfg), Version: 1}
	require.NoError(t, store.SeedMigrationRepairs(binding, []store.MigrationRepairObject{{JobID: "job", ObjectID: object.ID, Version: 1, ObjectJSON: repairJSON(object)}, {JobID: "job", ObjectID: table.ID, Version: 1, ObjectJSON: repairJSON(table)}}))
	report := datamigrate.MigrationReport{TriggerMigrationEnabled: true, Triggers: datamigrate.CategoryReport{Total: 1, Failed: 1, Items: []datamigrate.ObjectResult{{Name: object.Name, SourceName: object.SourceName, Table: object.SourceTable, Stage: "unsupported", SourceDDL: object.SourceDDL, Error: object.Error}}}}
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: "job", ReportJSON: repairJSON(report)}))
	reader := &triggerPlanTestReader{trigger: trigger}
	beforeReader, beforeWriter := newMigrationTriggerPlanReader, newMigrationTriggerPlanWriter
	newMigrationTriggerPlanReader = func(_ *store.Connection, db string, _ source.ConnPoolConfig, targets ...string) (source.Reader, error) {
		require.Equal(t, "Source", db)
		require.Equal(t, []string{"postgres"}, targets)
		return reader, nil
	}
	newMigrationTriggerPlanWriter = func(_ *store.Connection, schema string, _ target.ConnPoolConfig) (target.Writer, error) {
		require.Equal(t, "Target", schema)
		return &triggerPlanTestWriter{}, nil
	}
	t.Cleanup(func() { newMigrationTriggerPlanReader = beforeReader; newMigrationTriggerPlanWriter = beforeWriter })
	return object, reader
}

func previewTriggerPlan(t *testing.T) triggerPlanTestResponse {
	t.Helper()
	response := repairRequest(t, http.MethodPost, "/jobs/job/repairs/trg", `{"version":1}`, 1, RefreshMigrationTriggerPlan)
	require.Equal(t, 200, response.Code, response.Body.String())
	var result triggerPlanTestResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	return result
}
func saveTriggerPlanBody(plan triggerPlanTestResponse) string {
	return repairJSON(map[string]any{"version": plan.Version, "binding_version": plan.BindingVersion, "plan_hash": plan.PlanHash, "reviewed": true})
}

func TestTriggerPlanPreviewAndReviewedSavePreserveFrozenReport(t *testing.T) {
	object, reader := seedTriggerPlanHandler(t)
	before, err := store.GetMigrationRepairObject("job", "trg")
	require.NoError(t, err)
	report, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	plan := previewTriggerPlan(t)
	require.NotEmpty(t, plan.PlanHash)
	require.Equal(t, 1, plan.Version)
	require.Equal(t, 1, plan.BindingVersion)
	require.NotNil(t, plan.Object.MySQLPGTrigger)
	require.Len(t, plan.Object.Units, 2)
	require.Equal(t, "pending", plan.Object.Status)
	require.False(t, plan.Object.DiagnosticOnly)
	require.False(t, plan.Object.RequiresReview)
	require.Equal(t, object.OriginalError, plan.Object.OriginalError)
	require.Equal(t, []string{"table"}, plan.Object.Dependencies)
	for _, unit := range plan.Object.Units {
		require.True(t, unit.Selected)
		require.Equal(t, "pending", unit.Status)
		require.Equal(t, unit.SQL, unit.OriginalSQL)
	}
	after, err := store.GetMigrationRepairObject("job", "trg")
	require.NoError(t, err)
	require.Equal(t, before.ObjectJSON, after.ObjectJSON)
	require.Equal(t, 1, after.Version)
	response := repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", repairJSON(map[string]any{"version": 1, "binding_version": 1, "plan_hash": plan.PlanHash}), 1, SaveMigrationTriggerPlan)
	require.Equal(t, 400, response.Code)
	response = repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", saveTriggerPlanBody(plan), 1, SaveMigrationTriggerPlan)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Equal(t, 2, reader.reads)
	var saved triggerPlanTestResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &saved))
	require.Equal(t, 2, saved.Version)
	require.Equal(t, plan.Object.MySQLPGTrigger.FunctionSQL, saved.Object.MySQLPGTrigger.FunctionSQL)
	require.NoError(t, validateRepairUnits(saved.Object.Units))
	require.NoError(t, validateRepairObjectScope(saved.Object, saved.Object.Units, "postgres"))
	frozen, err := store.GetDataMigrationReport("job")
	require.NoError(t, err)
	require.Equal(t, report.ReportJSON, frozen.ReportJSON)
	var attempts int64
	require.NoError(t, store.DB.Model(&store.MigrationRepairAttempt{}).Count(&attempts).Error)
	require.Zero(t, attempts)
	response = repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", saveTriggerPlanBody(plan), 1, SaveMigrationTriggerPlan)
	require.Equal(t, 409, response.Code)
}

func TestTriggerPlanRejectsChangedSourceHashAndBinding(t *testing.T) {
	for _, change := range []string{"source", "metadata", "hash", "binding", "concurrent object", "concurrent binding", "dependency version", "concurrent dependency", "running attempt"} {
		t.Run(change, func(t *testing.T) {
			_, reader := seedTriggerPlanHandler(t)
			plan := previewTriggerPlan(t)
			switch change {
			case "source":
				reader.trigger.SourceDDL += " -- changed"
				reader.trigger.Body += " "
			case "metadata":
				reader.typ = "bigint"
			case "hash":
				plan.PlanHash = "forged"
			case "binding":
				require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Update("version", 2).Error)
			case "concurrent object":
				reader.afterRead = func() {
					require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "trg").Update("version", 2).Error)
				}
			case "concurrent binding":
				reader.afterRead = func() {
					require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Update("version", 2).Error)
				}
			case "dependency version":
				require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "table").Update("version", 2).Error)
			case "concurrent dependency":
				reader.afterRead = func() {
					require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "table").Update("version", 2).Error)
				}
			case "running attempt":
				require.NoError(t, store.DB.Create(&store.MigrationRepairAttempt{ID: "other", JobID: "job", ObjectID: "table", Kind: "ddl", Status: "running"}).Error)
			}
			response := repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", saveTriggerPlanBody(plan), 1, SaveMigrationTriggerPlan)
			require.Contains(t, []int{400, 409}, response.Code, response.Body.String())
			row, err := store.GetMigrationRepairObject("job", "trg")
			require.NoError(t, err)
			var current repair.Object
			require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &current))
			require.Nil(t, current.MySQLPGTrigger)
		})
	}
}

func TestTriggerPlanRejectsUntrustedOriginsAndOwnership(t *testing.T) {
	for _, change := range []string{"unknown", "running", "succeeded", "manual SQL", "units", "missing raw identity", "missing definition", "metadata failure", "wrong target", "execution history"} {
		t.Run(change, func(t *testing.T) {
			object, reader := seedTriggerPlanHandler(t)
			switch change {
			case "unknown", "running", "succeeded":
				object.Status = change
			case "manual SQL":
				object.DDL = "CREATE TRIGGER manual"
			case "units":
				object.Units = []repair.Unit{{SQL: "manual", Status: "pending"}}
			case "missing raw identity":
				object.SourceName = ""
			case "missing definition":
				object.SourceDDL = ""
				require.NoError(t, store.DB.Model(&store.DataMigrationReport{}).Where("job_id = ?", "job").Update("report_json", `{"trigger_migration_enabled":true}`).Error)
			case "metadata failure":
				report, _ := store.GetDataMigrationReport("job")
				var body datamigrate.MigrationReport
				require.NoError(t, json.Unmarshal([]byte(report.ReportJSON), &body))
				body.Triggers.Items[0].Stage = "metadata"
				require.NoError(t, store.DB.Model(&store.DataMigrationReport{}).Where("job_id = ?", "job").Update("report_json", repairJSON(body)).Error)
			case "wrong target":
				require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", "job").Update("dst_db_type", "mysql").Error)
			case "execution history":
				require.NoError(t, store.DB.Create(&store.MigrationRepairAttempt{ID: "past", JobID: "job", ObjectID: "trg", Kind: "ddl", Status: "failed"}).Error)
			}
			require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "trg").Update("object_json", repairJSON(object)).Error)
			response := repairRequest(t, http.MethodPost, "/jobs/job/repairs/trg", `{"version":1}`, 1, RefreshMigrationTriggerPlan)
			require.Equal(t, 400, response.Code, response.Body.String())
			require.Zero(t, reader.reads)
		})
	}
	_, reader := seedTriggerPlanHandler(t)
	response := repairRequest(t, http.MethodPost, "/jobs/job/repairs/trg", `{"version":1}`, 2, RefreshMigrationTriggerPlan)
	require.Equal(t, 404, response.Code)
	require.Zero(t, reader.reads)
}

func TestTriggerPlanCandidateCASFencesBindingAndRunningAttempts(t *testing.T) {
	for _, change := range []string{"binding", "running", "object", "dependency"} {
		t.Run(change, func(t *testing.T) {
			_, _ = seedTriggerPlanHandler(t)
			plan := previewTriggerPlan(t)
			switch change {
			case "binding":
				require.NoError(t, store.DB.Model(&store.MigrationRepairBinding{}).Where("job_id = ?", "job").Update("version", 2).Error)
			case "running":
				require.NoError(t, store.DB.Create(&store.MigrationRepairAttempt{ID: "other", JobID: "job", ObjectID: "table", Kind: "ddl", Status: "running"}).Error)
			case "object":
				require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "trg").Update("version", 2).Error)
			case "dependency":
				require.NoError(t, store.DB.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", "job", "table").Update("version", 2).Error)
			}
			require.ErrorIs(t, saveTriggerPlanCandidate("job", "trg", 1, 1, plan.Object, map[string]int{"table": 1}), store.ErrRepairConflict)
		})
	}
}

type triggerPlanExecuteWriter struct {
	triggerPlanTestWriter
	statements chan string
}

func (w *triggerPlanExecuteWriter) CreateMySQLPGTrigger(ctx context.Context, plan mysqltrigger.Plan, owner string) error {
	for _, sql := range plan.Statements() {
		if err := repair.Observe(ctx, sql, "running", nil); err != nil {
			return err
		}
		w.statements <- sql
		if err := repair.Observe(ctx, sql, "succeeded", nil); err != nil {
			return err
		}
	}
	return nil
}

func TestTriggerPlanSavedCandidateExecutesBothUnitsThroughExistingHandler(t *testing.T) {
	for _, change := range []string{"none", "data", "structure", "mapping", "types"} {
		t.Run(change, func(t *testing.T) {
			original, _ := seedTriggerPlanHandler(t)
			plan := previewTriggerPlan(t)
			saved := repairRequest(t, http.MethodPut, "/jobs/job/repairs/trg", saveTriggerPlanBody(plan), 1, SaveMigrationTriggerPlan)
			require.Equal(t, 200, saved.Code, saved.Body.String())
			if change != "none" {
				row, err := store.GetMigrationRepairObject("job", "table")
				require.NoError(t, err)
				var dependency repair.Object
				require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &dependency))
				switch change {
				case "data":
					dependency.DataStatus = "failed"
				case "structure":
					dependency.Status = "failed"
				case "mapping":
					dependency.ColumnMapping["Value"] = "Other"
				case "types":
					dependency.Columns[0].DataType = "BIGINT"
				}
				require.NoError(t, store.SaveMigrationRepairObject("job", "table", row.Version, repairJSON(dependency)))
			}
			statements := make(chan string, 2)
			previous := newMigrationRepairWriter
			newMigrationRepairWriter = func(*store.Connection, string, target.ConnPoolConfig) (target.Writer, error) {
				return &triggerPlanExecuteWriter{statements: statements}, nil
			}
			t.Cleanup(func() { newMigrationRepairWriter = previous })
			response := repairRequest(t, http.MethodPost, "/jobs/job/repairs/trg", `{"version":2,"confirmed":true,"request_id":"execute-new-plan"}`, 1, ExecuteMigrationRepair)
			if change != "none" {
				require.Equal(t, 400, response.Code, response.Body.String())
				require.Empty(t, statements)
				return
			}
			require.Equal(t, 202, response.Code, response.Body.String())
			var pending struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &pending))
			require.NotEmpty(t, pending.ID)
			var attempt store.MigrationRepairAttempt
			require.Eventually(t, func() bool {
				return store.DB.First(&attempt, "id = ?", pending.ID).Error == nil && attempt.Status != "running"
			}, 3*time.Second, 10*time.Millisecond)
			require.Equal(t, "succeeded", attempt.Status, attempt.Error)
			require.Len(t, statements, 2)
			require.Equal(t, plan.Object.MySQLPGTrigger.FunctionSQL, <-statements)
			require.Equal(t, plan.Object.MySQLPGTrigger.TriggerSQL, <-statements)
			row, err := store.GetMigrationRepairObject("job", "trg")
			require.NoError(t, err)
			var current repair.Object
			require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &current))
			require.Equal(t, "succeeded", current.Status)
			require.Equal(t, original.OriginalError, current.OriginalError)
			for _, unit := range current.Units {
				require.Equal(t, "succeeded", unit.Status)
				require.False(t, unit.Selected)
			}
			require.True(t, current.MySQLPGTrigger.Version >= 2, fmt.Sprint(current.MySQLPGTrigger.Version))
		})
	}
}
