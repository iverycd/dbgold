package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

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
