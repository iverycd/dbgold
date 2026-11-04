package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/validation"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// Explicitly supply two disposable local instances. The fixture creates its
// own source database and a uniquely named target table in the login schema.
func TestDamengWriteDefaultAPIIntegration(t *testing.T) {
	myDSN, dmDSN := os.Getenv("DBGOLD_API_TEST_MYSQL_DSN"), os.Getenv("DBGOLD_API_TEST_DM_DSN")
	if myDSN == "" || dmDSN == "" {
		t.Skip("requires dedicated DBGOLD_API_TEST_MYSQL_DSN and DBGOLD_API_TEST_DM_DSN")
	}
	myCfg, err := mysql.ParseDSN(myDSN)
	require.NoError(t, err)
	myHost, myPort, err := net.SplitHostPort(myCfg.Addr)
	require.NoError(t, err)
	dmURL, err := url.Parse(dmDSN)
	require.NoError(t, err)
	require.NotNil(t, dmURL.User)
	dmPass, _ := dmURL.User.Password()
	myPortNumber, err := strconv.Atoi(myPort)
	require.NoError(t, err)
	dmPort, err := strconv.Atoi(dmURL.Port())
	require.NoError(t, err)
	sourceDB := fmt.Sprintf("dbgold_api_dm_%d", time.Now().UnixNano())
	table := fmt.Sprintf("API_DM_%d", time.Now().UnixNano())
	my, err := sql.Open("mysql", myDSN)
	require.NoError(t, err)
	defer my.Close()
	dm, err := sql.Open("dm", dmDSN)
	require.NoError(t, err)
	defer dm.Close()
	_, err = my.Exec("CREATE DATABASE " + sourceDB + " CHARACTER SET utf8mb4")
	require.NoError(t, err)
	defer func() { _, err := my.Exec("DROP DATABASE " + sourceDB); require.NoError(t, err) }()
	_, err = my.Exec("CREATE TABLE " + sourceDB + "." + table + " (ID INT PRIMARY KEY, LABEL VARCHAR(48) NULL)")
	require.NoError(t, err)
	_, err = my.Exec("INSERT INTO " + sourceDB + "." + table + " VALUES(1,'one'),(2,'二'),(3,NULL),(4,'four'),(5,'five')")
	require.NoError(t, err)
	schema := dmURL.User.Username()
	qTable := fmt.Sprintf(`"%s"."%s"`, schema, table)
	_, err = dm.Exec("CREATE TABLE " + qTable + " (ID INT NOT NULL PRIMARY KEY, LABEL VARCHAR(48))")
	require.NoError(t, err)
	defer func() { _, err := dm.Exec("DROP TABLE " + qTable); require.NoError(t, err) }()

	migrationHandlerTestDB(t)
	require.NoError(t, store.DB.AutoMigrate(&store.BatchMigration{}, &store.DataMigrationReport{}, &store.MigrationRepairBinding{}, &store.MigrationRepairLock{}, &store.MigrationRepairObject{}))
	src := &store.Connection{OwnerID: 7, Name: "fixture-source", DBType: "mysql", Host: myHost, Port: myPortNumber, Database: sourceDB, Username: myCfg.User, Password: myCfg.Passwd}
	dst := &store.Connection{OwnerID: 7, Name: "fixture-target", DBType: "dameng", Host: dmURL.Hostname(), Port: dmPort, Database: schema, Username: dmURL.User.Username(), Password: dmPass}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", uint(7)); c.Set("role", "user"); c.Next() })
	router.POST("/preflight", PreflightDataMigration)
	router.POST("/start", StartDataMigration)
	router.POST("/batch", StartBatch)
	router.GET("/history/:jobID", GetDataMigrationJobDetail)
	call := func(t *testing.T, req *http.Request) []byte {
		t.Helper()
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		require.Equal(t, http.StatusOK, out.Code, out.Body.String())
		return out.Body.Bytes()
	}
	check := func(t *testing.T, jobID, want string) {
		t.Helper()
		require.Eventually(t, func() bool { return datamigrate.Registry.Get(jobID) == nil }, 30*time.Second, 20*time.Millisecond)
		job, err := store.GetDataMigrationJob(jobID)
		require.NoError(t, err)
		require.Equal(t, "done", job.Status, job.Summary)
		var frozen datamigrate.Config
		require.NoError(t, json.Unmarshal([]byte(job.RepairConfigJSON), &frozen))
		require.Equal(t, want, frozen.DamengWriteStrategy)
		var history struct {
			Strategy string `json:"dameng_write_strategy"`
		}
		require.NoError(t, json.Unmarshal(call(t, httptest.NewRequest(http.MethodGet, "/history/"+jobID, nil)), &history))
		require.Equal(t, want, history.Strategy)
		saved, err := store.GetDataMigrationReport(jobID)
		require.NoError(t, err)
		var report datamigrate.MigrationReport
		require.NoError(t, json.Unmarshal([]byte(saved.ReportJSON), &report))
		require.Equal(t, 1, report.Data.Success)
		require.NotNil(t, report.DataValidation)
		require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
		var n int
		require.NoError(t, dm.QueryRow("SELECT COUNT(*) FROM "+qTable).Scan(&n))
		require.Equal(t, 5, n)
		var value string
		require.NoError(t, dm.QueryRow("SELECT LABEL FROM "+qTable+" WHERE ID=2").Scan(&value))
		require.Equal(t, "二", value)
		_, err = dm.Exec("DELETE FROM " + qTable)
		require.NoError(t, err)
	}
	for _, want := range []string{"batch", "row"} {
		t.Run("single_"+want, func(t *testing.T) {
			body := map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "migrate_mode": "all", "migrate_content": "data_only", "target_schema": schema, "page_size": 5, "max_parallel": 1, "intra_table_parallel": 1, "validate_data": true}
			if want == "row" {
				body["dameng_write_strategy"] = want
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			request := func(path string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				return req
			}
			var preflight struct {
				Ready bool `json:"ready"`
			}
			require.NoError(t, json.Unmarshal(call(t, request("/preflight")), &preflight))
			require.True(t, preflight.Ready)
			var started struct {
				JobID string `json:"job_id"`
			}
			require.NoError(t, json.Unmarshal(call(t, request("/start")), &started))
			require.NotEmpty(t, started.JobID)
			check(t, started.JobID, want)
		})
	}
	t.Run("excel_omitted", func(t *testing.T) {
		workbook := excelize.NewFile()
		defer workbook.Close()
		header := batchHeaders
		values := []any{"mysql", src.Host, src.Port, src.Database, src.Username, src.Password, "dameng", dst.Host, dst.Port, dst.Database, dst.Username, dst.Password, schema}
		require.NoError(t, workbook.SetSheetRow("Sheet1", "A1", &header))
		require.NoError(t, workbook.SetSheetRow("Sheet1", "A2", &values))
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		file, err := form.CreateFormFile("file", "fixture.xlsx")
		require.NoError(t, err)
		require.NoError(t, workbook.Write(file))
		require.NoError(t, form.WriteField("migrate_content", "data_only"))
		require.NoError(t, form.WriteField("validate_data", "true"))
		require.NoError(t, form.Close())
		req := httptest.NewRequest(http.MethodPost, "/batch", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		var started struct {
			BatchID string `json:"batch_id"`
		}
		require.NoError(t, json.Unmarshal(call(t, req), &started))
		require.NotEmpty(t, started.BatchID)
		require.Eventually(t, func() bool {
			b, err := store.GetBatchMigration(started.BatchID)
			return err == nil && b.Status == "done"
		}, 30*time.Second, 20*time.Millisecond)
		jobs, err := store.ListJobsByBatch(started.BatchID)
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		check(t, jobs[0].JobID, "batch")
	})
}
