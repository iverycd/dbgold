package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestDamengCommentRepairAPIIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_API_TEST_DM_DSN")
	if dsn == "" {
		t.Skip("requires a disposable Dameng target in DBGOLD_API_TEST_DM_DSN")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	defer db.Close()
	schema := strings.ToUpper(parsed.User.Username())
	table := fmt.Sprintf("cg_projectdailichouqu_%d", time.Now().UnixNano())
	qualified := dialect.NewDaMeng().QualifyTable(schema, table)
	_, err = db.Exec("CREATE TABLE " + qualified + " (DAILIPHONE VARCHAR(32))")
	require.NoError(t, err)
	defer func() { _, err := db.Exec("DROP TABLE " + qualified); require.NoError(t, err) }()

	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: filepath.Join(t.TempDir(), "repair.db")}))
	password, _ := parsed.User.Password()
	dst := &store.Connection{OwnerID: 1, Name: "dm", DBType: "dameng", Host: parsed.Hostname(), Port: port, Username: parsed.User.Username(), Password: password}
	require.NoError(t, store.CreateConnection(dst))
	job := &store.DataMigrationJob{JobID: "comment-test", OwnerID: 1, Status: "failed", SrcDBType: "mysql", DstDBType: "dameng", DstConnID: dst.ID, DstSchema: schema}
	require.NoError(t, store.CreateDataMigrationJob(job))
	tableObj := repair.Object{ID: "table", Kind: "table", Table: table, TargetName: table, Schema: schema, Status: "succeeded", DataStatus: "failed"}
	ddl := "COMMENT ON COLUMN " + qualified + `."DAILIPHONE" IS 'dailiphone'`
	cm := repair.Object{ID: "comment", Kind: "comment", Schema: schema, Table: table, TargetName: table + ".DAILIPHONE",
		DDL: ddl, Status: "blocked", Dependencies: []string{tableObj.ID}, Units: []repair.Unit{{ID: "step-1", OriginalSQL: ddl, SQL: ddl, Selected: true, Status: "blocked"}}}
	report := datamigrate.MigrationReport{
		Data:           datamigrate.CategoryReport{Failed: 1, Items: []datamigrate.ObjectResult{{Name: table, Error: "Error -2665: 记录超长"}}},
		RepairManifest: &datamigrate.RepairManifest{Config: datamigrate.Config{TargetSchema: schema}, Objects: []repair.Object{tableObj, cm}},
	}
	require.NoError(t, store.CreateDataMigrationReport(&store.DataMigrationReport{JobID: job.JobID, ReportJSON: repairJSON(report)}))
	path := "/jobs/comment-test/repairs/comment"
	saved := repairRequest(t, http.MethodPut, path, repairJSON(map[string]any{"version": 1, "units": cm.Units, "reviewed": true}), 1, SaveMigrationRepairDraft)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	run := repairRequest(t, http.MethodPost, path, `{"version":2,"confirmed":true,"request_id":"retry-comment"}`, 1, ExecuteMigrationRepair)
	require.Equal(t, http.StatusAccepted, run.Code, run.Body.String())
	var attempt store.MigrationRepairAttempt
	require.Eventually(t, func() bool {
		return store.DB.Where("request_id = ?", "retry-comment").First(&attempt).Error == nil && attempt.Status != "running"
	}, 15*time.Second, 20*time.Millisecond)
	require.Equal(t, "succeeded", attempt.Status, attempt.Error)
	var text string
	require.NoError(t, db.QueryRow("SELECT COMMENTS FROM ALL_COL_COMMENTS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME=?", schema, strings.ToUpper(table), "DAILIPHONE").Scan(&text))
	require.Equal(t, "dailiphone", text)
	row, err := store.GetMigrationRepairObject(job.JobID, tableObj.ID)
	require.NoError(t, err)
	require.JSONEq(t, repairJSON(tableObj), row.ObjectJSON)
	history, err := store.GetDataMigrationReport(job.JobID)
	require.NoError(t, err)
	require.Equal(t, repairJSON(report), history.ReportJSON)
}

// Opt-in end-to-end exercise of the public handlers and durable async workers.
func TestRepairMySQLAPIIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_REPAIR_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_REPAIR_MYSQL_DSN for disposable repair integration databases")
	}
	parsed, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(parsed.Addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	srcName, dstName := "repair_api_src_"+suffix, "repair_api_dst_"+suffix
	for _, name := range []string{srcName, dstName} {
		_, err = db.Exec("CREATE DATABASE `" + name + "`")
		require.NoError(t, err)
		defer db.Exec("DROP DATABASE `" + name + "`")
	}
	_, err = db.Exec("CREATE TABLE `" + srcName + "`.Orders (ID INT NOT NULL PRIMARY KEY, Name VARCHAR(40) NOT NULL)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO `" + srcName + "`.Orders VALUES (1,'one'),(2,'two'),(3,'three')")
	require.NoError(t, err)
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: filepath.Join(t.TempDir(), "repairs.db")}))
	src := &store.Connection{OwnerID: 1, Name: "source", DBType: "mysql", Host: host, Port: port, Database: srcName, Username: parsed.User, Password: parsed.Passwd}
	dst := &store.Connection{OwnerID: 1, Name: "target", DBType: "mysql", Host: host, Port: port, Database: dstName, Username: parsed.User, Password: parsed.Passwd}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	cfg := datamigrate.Config{Content: "both", TargetDBType: "mysql", TargetSchema: dstName, PageSize: 2}
	job := &store.DataMigrationJob{JobID: "api-integration", OwnerID: 1, Status: "failed", SrcConnID: src.ID, DstConnID: dst.ID, SrcDBType: "mysql", DstDBType: "mysql", DstSchema: dstName, SourceDatabase: srcName, SourceConnectionDatabase: srcName, RepairConfigJSON: repairJSON(cfg)}
	require.NoError(t, store.CreateDataMigrationJob(job))
	r, err := buildSrcReader(src, srcName, source.ConnPoolConfig{}, "mysql")
	require.NoError(t, err)
	defer r.Close()
	info, err := r.GetTableDDLInfo(context.Background(), "Orders")
	require.NoError(t, err)
	bad := "CREATE TABLE `" + dstName + "`.Orders (ID INT, Renamed invalid_type)"
	obj := repair.Object{ID: "table", Kind: "table", Name: "Orders", SourceName: "Orders", SourceTable: "Orders", Table: "Orders", TargetName: "Orders", Schema: dstName, Status: "failed", OriginalStatus: "failed", OriginalError: "invalid type", Error: "invalid type", DDL: bad, DataStatus: "pending", SourceColumns: info.Columns, ColumnMapping: map[string]string{"ID": "ID", "Name": "Renamed"}, Units: []repair.Unit{{ID: "create", SQL: bad, OriginalSQL: bad, Status: "failed", Selected: true}}}
	idx := repair.Object{ID: "index", Kind: "index", Name: "ix_name", Table: "Orders", SourceTable: "Orders", TargetName: "ix_name", Schema: dstName, Status: "pending", OriginalStatus: "pending", Dependencies: []string{"table"}, Index: &source.IndexInfo{TableName: "Orders", IndexName: "ix_name", Columns: []string{"Renamed"}}, Units: []repair.Unit{{ID: "index-create", SQL: "ALTER TABLE `" + dstName + "`.Orders ADD INDEX ix_name(Missing)", Status: "pending", Selected: true}}}
	binding := &store.MigrationRepairBinding{JobID: job.JobID, SrcConnID: src.ID, DstConnID: dst.ID, SourceDatabase: srcName, TargetSchema: dstName, SourceFingerprint: connectionFingerprint(src), TargetFingerprint: connectionFingerprint(dst), Configured: true, ConfigJSON: repairJSON(cfg), Version: 1}
	require.NoError(t, store.SeedMigrationRepairs(binding, []store.MigrationRepairObject{{JobID: job.JobID, ObjectID: obj.ID, ObjectJSON: repairJSON(obj), Version: 1}, {JobID: job.JobID, ObjectID: idx.ID, ObjectJSON: repairJSON(idx), Version: 1}}))
	path := "/jobs/" + job.JobID + "/repairs/"
	call := func(object string, body any, method string, handler func(*gin.Context)) *httptest.ResponseRecorder {
		return repairRequest(t, method, path+object, repairJSON(body), 1, handler)
	}
	wait := func(id string) store.MigrationRepairAttempt {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var a store.MigrationRepairAttempt
			require.NoError(t, store.DB.First(&a, "id = ?", id).Error)
			if a.Status != "running" {
				return a
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("repair worker did not finish")
		return store.MigrationRepairAttempt{}
	}
	start := func(object string, body any, handler func(*gin.Context)) store.MigrationRepairAttempt {
		response := call(object, body, http.MethodPost, handler)
		require.Equal(t, 202, response.Code, response.Body.String())
		var answer struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &answer))
		return wait(answer.ID)
	}
	obj.Units[0].SQL = "CREATE TABLE `" + dstName + "`.Orders (ID INT NOT NULL PRIMARY KEY, Renamed VARCHAR(80) NOT NULL)"
	saved := call("table", map[string]any{"version": 1, "units": obj.Units, "column_mapping": obj.ColumnMapping, "reviewed": true}, http.MethodPut, SaveMigrationRepairDraft)
	require.Equal(t, 200, saved.Code, saved.Body.String())
	executeBody := map[string]any{"version": 2, "request_id": "create-once", "confirmed": true}
	attempt := start("table", executeBody, ExecuteMigrationRepair)
	require.Equal(t, "succeeded", attempt.Status, attempt.Error)
	replay := start("table", executeBody, ExecuteMigrationRepair)
	require.Equal(t, attempt.ID, replay.ID)
	preview := func() (string, repair.RecoveryPreview) {
		row, e := store.GetMigrationRepairObject(job.JobID, "table")
		require.NoError(t, e)
		response := call("table", map[string]any{"version": row.Version, "related_object_ids": []string{"index"}}, http.MethodPost, PreviewMigrationRecovery)
		require.Equal(t, 200, response.Code, response.Body.String())
		var result struct {
			Token   string                 `json:"token"`
			Preview repair.RecoveryPreview `json:"preview"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		return result.Token, result.Preview
	}
	token, p := preview()
	require.True(t, p.DataRequired)
	body := map[string]any{"token": token, "request_id": "data-once", "confirmed": true, "source_quiesced": true, "target_quiesced": true}
	attempt = start("table", body, ExecuteMigrationRecovery)
	require.Equal(t, "failed", attempt.Status)
	var result repair.RecoveryResult
	require.NoError(t, json.Unmarshal([]byte(attempt.ResultJSON), &result))
	require.Equal(t, "succeeded", result.DataStatus)
	row, err := store.GetMigrationRepairObject(job.JobID, "index")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(row.ObjectJSON), &idx))
	idx.Units[0].SQL = "ALTER TABLE `" + dstName + "`.Orders ADD INDEX ix_name(Renamed)"
	saved = call("index", map[string]any{"version": row.Version, "units": idx.Units, "reviewed": true}, http.MethodPut, SaveMigrationRepairDraft)
	require.Equal(t, 200, saved.Code, saved.Body.String())
	token, p = preview()
	require.False(t, p.DataRequired)
	body["token"], body["request_id"] = token, "index-only"
	attempt = start("table", body, ExecuteMigrationRecovery)
	require.Equal(t, "succeeded", attempt.Status, attempt.Error)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM `"+dstName+"`.Orders").Scan(&count))
	require.Equal(t, 3, count)
	unchanged, err := store.GetDataMigrationJob(job.JobID)
	require.NoError(t, err)
	require.Equal(t, "failed", unchanged.Status)
}

func TestMySQLSelectedViewsRepairJobIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_REPAIR_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set DBGOLD_REPAIR_MYSQL_DSN for disposable view databases")
	}
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	host, portString, err := net.SplitHostPort(cfg.Addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: filepath.Join(t.TempDir(), "view-repair.db")}))

	createConnection := func(prefix string) *store.Connection {
		name := fmt.Sprintf("dbgold_%s_%d", prefix, time.Now().UnixNano())
		_, err := db.Exec("CREATE DATABASE " + name)
		require.NoError(t, err)
		conn := &store.Connection{OwnerID: 1, Name: name, DBType: "mysql", Host: host, Port: port, Database: name, Username: cfg.User, Password: cfg.Passwd}
		require.NoError(t, store.CreateConnection(conn))
		return conn
	}
	src := createConnection("src")
	defer db.Exec("DROP DATABASE " + src.Database)
	dst := createConnection("dst")
	defer db.Exec("DROP DATABASE " + dst.Database)
	for _, stmt := range []string{
		"CREATE TABLE " + src.Database + ".base (id INTEGER)",
		"CREATE VIEW " + src.Database + ".broken AS SELECT id FROM " + src.Database + ".base",
		"CREATE VIEW " + src.Database + ".good AS SELECT 42 AS answer",
		"CREATE VIEW " + src.Database + ".unselected AS SELECT 0 AS unused",
	} {
		_, err = db.Exec(stmt)
		require.NoError(t, err)
	}
	body := repairJSON(migrateViewsRequest{SrcConnID: src.ID, DstConnID: dst.ID, TargetSchema: dst.Database, ViewNames: []string{"broken", "good"}})
	resp := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(resp)
	c.Set("userID", uint(1))
	c.Set("role", "user")
	c.Request = httptest.NewRequest(http.MethodPost, "/migration/view-migrate", strings.NewReader(body))
	MigrateViews(c)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var outcome struct {
		JobID   string                     `json:"job_id"`
		Results []datamigrate.ObjectResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &outcome))
	require.NotEmpty(t, outcome.JobID)
	require.Len(t, outcome.Results, 2)
	job, err := store.GetDataMigrationJob(outcome.JobID)
	require.NoError(t, err)
	require.Equal(t, "failed", job.Status)
	require.Equal(t, dst.Database, job.DstSchema)
	report, err := store.GetDataMigrationReport(job.JobID)
	require.NoError(t, err)
	var original datamigrate.MigrationReport
	require.NoError(t, json.Unmarshal([]byte(report.ReportJSON), &original))
	require.Equal(t, 1, original.Views.Success)
	require.Equal(t, 1, original.Views.Failed)
	var targetObjects string
	require.NoError(t, db.QueryRow("SELECT GROUP_CONCAT(TABLE_NAME) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?", dst.Database).Scan(&targetObjects))
	require.Equal(t, "good", targetObjects)
	ledger, _, err := repairObjects(job.JobID)
	require.NoError(t, err)
	require.Len(t, ledger, 2)
	var failed repair.Object
	for _, o := range ledger {
		if o.TargetName == "broken" {
			failed = o
		}
	}
	require.Equal(t, "failed", failed.Status)
	require.Len(t, failed.Units, 1)
	require.Equal(t, failed.DDL, failed.Units[0].SQL)
	_, err = db.Exec("CREATE TABLE " + dst.Database + ".base (id INTEGER)")
	require.NoError(t, err)
	path := "/jobs/" + job.JobID + "/repairs/" + failed.ID
	saved := repairRequest(t, http.MethodPut, path, repairJSON(map[string]any{"version": 1, "units": failed.Units, "reviewed": true}), 1, SaveMigrationRepairDraft)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	started := repairRequest(t, http.MethodPost, path, repairJSON(map[string]any{"version": 2, "request_id": "unchanged-view", "confirmed": true}), 1, ExecuteMigrationRepair)
	require.Equal(t, http.StatusAccepted, started.Code, started.Body.String())
	var attempt store.MigrationRepairAttempt
	require.Eventually(t, func() bool {
		return store.DB.Where("job_id = ? AND request_id = ?", job.JobID, "unchanged-view").First(&attempt).Error == nil && attempt.Status != "running"
	}, 10*time.Second, 10*time.Millisecond)
	require.Equal(t, "succeeded", attempt.Status, attempt.Error)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+dst.Database+".broken").Scan(&count))
	after, err := store.GetDataMigrationReport(job.JobID)
	require.NoError(t, err)
	require.Equal(t, report.ReportJSON, after.ReportJSON)
}
