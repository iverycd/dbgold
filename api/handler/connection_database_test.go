package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"dbgold/config"
	"dbgold/datamigrate/cdc"
	"dbgold/store"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/microsoft/go-mssqldb/msdsn"
	"github.com/stretchr/testify/require"
)

func seedDatabaseConnection(t *testing.T) *store.Connection {
	t.Helper()
	require.NoError(t, store.InitWithError(&config.Config{SQLitePath: ":memory:"}))
	conn := &store.Connection{OwnerID: 1, Name: "saved", DBType: "postgres", Host: "db", Port: 5432,
		Database: "Original", Username: "reader", Password: "private-password", Env: "test"}
	require.NoError(t, store.CreateConnection(conn))
	return conn
}

func databaseConnectionRequest(t *testing.T, method, suffix, body string, id, owner uint, role string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Handle(method, "/connections/:id"+suffix, func(c *gin.Context) {
		c.Set("userID", owner)
		c.Set("role", role)
		handler(c)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, fmt.Sprintf("/connections/%d%s", id, suffix), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func patchDatabase(t *testing.T, conn *store.Connection, body string, check func(context.Context, *store.Connection) error) *httptest.ResponseRecorder {
	t.Helper()
	return databaseConnectionRequest(t, http.MethodPatch, "/database", body, conn.ID, 1, "user", func(c *gin.Context) {
		updateConnectionDatabase(c, check)
	})
}

const databaseEditBody = `{"database":"Selected DB","expected_database":"Original"}`

func TestConnectionDatabaseSaveTestsCandidateAndPreservesOtherFields(t *testing.T) {
	conn := seedDatabaseConnection(t)
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{
		JobID: "historical", SrcConnID: conn.ID, SrcConnName: conn.Name, SrcConnDatabase: conn.Database, Status: "failed",
	}))
	called := false
	response := patchDatabase(t, conn, databaseEditBody, func(ctx context.Context, candidate *store.Connection) error {
		called = true
		require.Equal(t, "Selected DB", candidate.Database)
		require.Equal(t, conn.Password, candidate.Password)
		persisted, err := store.GetConnection(conn.ID)
		require.NoError(t, err)
		require.Equal(t, "Original", persisted.Database, "testing must not save the candidate")
		// An unrelated metadata edit during the test must not be overwritten.
		return store.UpdateConnection(conn.ID, map[string]any{"name": "new display name", "env": "production"})
	})
	require.True(t, called)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), "password")
	var returned store.Connection
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &returned))
	require.Equal(t, "Selected DB", returned.Database)
	require.Equal(t, "new display name", returned.Name)
	require.Equal(t, "production", returned.Env)
	persisted, err := store.GetConnection(conn.ID)
	require.NoError(t, err)
	require.Equal(t, conn.Password, persisted.Password)
	require.Equal(t, conn.Host, persisted.Host)
	require.Equal(t, conn.Username, persisted.Username)
	history, err := store.GetDataMigrationJob("historical")
	require.NoError(t, err)
	require.Equal(t, "Original", history.SrcConnDatabase)
}

func TestConnectionDatabaseFailedTestDoesNotSave(t *testing.T) {
	conn := seedDatabaseConnection(t)
	response := patchDatabase(t, conn, databaseEditBody, func(context.Context, *store.Connection) error {
		return errors.New("permission denied")
	})
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Contains(t, response.Body.String(), "未保存")
	persisted, err := store.GetConnection(conn.ID)
	require.NoError(t, err)
	require.Equal(t, "Original", persisted.Database)
}

func TestConnectionDatabaseRejectsInvalidOrStaleEditor(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing expected value", `{"database":"next"}`, 400},
		{"null expected value", `{"database":"next","expected_database":null}`, 400},
		{"blank name", `{"database":"  ","expected_database":"Original"}`, 400},
		{"control character", `{"database":"next\u0000name","expected_database":"Original"}`, 400},
		{"stale editor", `{"database":"next","expected_database":"older"}`, 409},
		{"case remains significant", `{"database":"next","expected_database":"original"}`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := seedDatabaseConnection(t)
			response := patchDatabase(t, conn, tc.body, func(context.Context, *store.Connection) error {
				t.Fatal("invalid requests must not contact the database")
				return nil
			})
			require.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
}

func TestConnectionDatabasePreservesCaseAndAllowsEmptyExpectedDefault(t *testing.T) {
	conn := seedDatabaseConnection(t)
	require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{"database": ""}))
	response := patchDatabase(t, conn, `{"database":"  MiXeD DB  ","expected_database":""}`, func(_ context.Context, candidate *store.Connection) error {
		require.Equal(t, "MiXeD DB", candidate.Database)
		return nil
	})
	require.Equal(t, 200, response.Code, response.Body.String())
}

func TestConnectionDatabaseDetectsChangesDuringTest(t *testing.T) {
	for _, field := range []string{"database", "host", "port", "db_type", "username", "password", "owner_id", "deleted"} {
		t.Run(field, func(t *testing.T) {
			conn := seedDatabaseConnection(t)
			response := patchDatabase(t, conn, databaseEditBody, func(context.Context, *store.Connection) error {
				if field == "deleted" {
					return store.DeleteConnection(conn.ID)
				}
				var value any = "changed"
				if field == "port" || field == "owner_id" {
					value = 2
				}
				return store.DB.Model(&store.Connection{}).Where("id = ?", conn.ID).Update(field, value).Error
			})
			require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		})
	}
}

func TestConnectionDatabaseEnforcesOwnershipAndDatabaseCapabilities(t *testing.T) {
	conn := seedDatabaseConnection(t)
	called := false
	check := func(context.Context, *store.Connection) error { called = true; return nil }
	handler := func(c *gin.Context) { updateConnectionDatabase(c, check) }
	response := databaseConnectionRequest(t, http.MethodPatch, "/database", databaseEditBody, conn.ID, 2, "user", handler)
	require.Equal(t, 404, response.Code)
	require.False(t, called)
	response = databaseConnectionRequest(t, http.MethodPatch, "/database", databaseEditBody, conn.ID, 2, "admin", handler)
	require.Equal(t, 200, response.Code)
	require.True(t, called)
	for _, typ := range []string{"oracle", "dameng"} {
		require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{"db_type": typ}))
		response = patchDatabase(t, conn, databaseEditBody, check)
		require.Equal(t, 400, response.Code)
	}
}

func TestConnectionDatabaseBlocksBothSidesOfUnfinishedIncrementalJobs(t *testing.T) {
	statuses := []string{"initializing", "running", "snapshot", "paused_manual", "paused_restart", "paused_ddl", "failed", "bootstrap_review", "cutover_blocked", "stopped", "aborted"}
	for _, side := range []string{"source", "target"} {
		for _, status := range statuses {
			t.Run(side+"/"+status, func(t *testing.T) {
				conn := seedDatabaseConnection(t)
				job := &store.IncrementalMigrationJob{JobID: "cdc", Status: status}
				if side == "source" {
					job.SrcConnID = conn.ID
				} else {
					job.DstConnID = conn.ID
				}
				require.NoError(t, store.CreateIncrementalJob(job))
				called := false
				response := patchDatabase(t, conn, databaseEditBody, func(context.Context, *store.Connection) error { called = true; return nil })
				if status == "stopped" || status == "aborted" {
					require.Equal(t, 200, response.Code, response.Body.String())
					require.True(t, called)
				} else {
					require.Equal(t, 409, response.Code, response.Body.String())
					require.False(t, called)
				}
			})
		}
	}
}

func TestConnectionDatabaseUnchangedValueDoesNotTestOrBlock(t *testing.T) {
	conn := seedDatabaseConnection(t)
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{JobID: "cdc", SrcConnID: conn.ID, Status: "running"}))
	response := patchDatabase(t, conn, `{"database":"Original","expected_database":"Original"}`, func(context.Context, *store.Connection) error {
		t.Fatal("saving the unchanged database must not contact it")
		return nil
	})
	require.Equal(t, 200, response.Code)
}

func TestFullConnectionUpdateCannotBypassIncrementalDatabaseGuard(t *testing.T) {
	conn := seedDatabaseConnection(t)
	require.NoError(t, store.CreateIncrementalJob(&store.IncrementalMigrationJob{JobID: "cdc", DstConnID: conn.ID, Status: "paused_restart"}))
	form := updateConnectionRequest{Name: conn.Name, DBType: conn.DBType, Host: conn.Host, Port: conn.Port, Database: "other", Username: conn.Username}
	payload, err := json.Marshal(form)
	require.NoError(t, err)
	response := databaseConnectionRequest(t, http.MethodPut, "", string(payload), conn.ID, 1, "user", UpdateConnection)
	require.Equal(t, 409, response.Code, response.Body.String())
	form.Database, form.Name = "Original", "renamed"
	payload, err = json.Marshal(form)
	require.NoError(t, err)
	response = databaseConnectionRequest(t, http.MethodPut, "", string(payload), conn.ID, 1, "user", UpdateConnection)
	require.Equal(t, 200, response.Code)
}

func TestDatabaseSaveAndCDCStartCannotCrossPreflightSnapshots(t *testing.T) {
	for _, startFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("start_first=%t", startFirst), func(t *testing.T) {
			dst := seedDatabaseConnection(t)
			src := &store.Connection{OwnerID: 1, DBType: "mysql", Database: "source", Name: "source"}
			require.NoError(t, store.CreateConnection(src))
			job := &store.IncrementalMigrationJob{JobID: "new-cdc", SrcConnID: src.ID, DstConnID: dst.ID, TargetSchema: "public", Status: "initializing"}
			response := patchDatabase(t, dst, databaseEditBody, func(context.Context, *store.Connection) error {
				if startFirst {
					require.NoError(t, createIncrementalAfterPreflight(job, src, dst, func() {}))
				}
				return nil
			})
			if startFirst {
				require.Equal(t, 409, response.Code, response.Body.String())
				frozen, err := store.GetConnection(dst.ID)
				require.NoError(t, err)
				require.Equal(t, "Original", frozen.Database)
			} else {
				require.Equal(t, 200, response.Code, response.Body.String())
				require.ErrorIs(t, createIncrementalAfterPreflight(job, src, dst, func() {
					t.Error("a stale preflight must not launch a CDC runner")
				}), store.ErrConnectionChanged)
				var count int64
				require.NoError(t, store.DB.Model(&store.IncrementalMigrationJob{}).Count(&count).Error)
				require.Zero(t, count, "stale preflight must not create a CDC task")
			}
		})
	}
}

func TestTicketConnectionReuseWaitsForCDCRegistration(t *testing.T) {
	dst := seedDatabaseConnection(t)
	src := &store.Connection{OwnerID: 1, DBType: "mysql", Database: "source", Name: "source"}
	require.NoError(t, store.CreateConnection(src))
	job := &store.IncrementalMigrationJob{JobID: "ticket-race", SrcConnID: src.ID, DstConnID: dst.ID, TargetSchema: "public", Status: "initializing"}
	attempted := make(chan struct{})
	result := make(chan error, 1)
	require.NoError(t, createIncrementalAfterPreflight(job, src, dst, func() {
		go func() {
			close(attempted)
			_, err := upsertTicketConn(dst.OwnerID, dst.Name, dst.DBType, dst.Host, dst.Port, "from-ticket", dst.Username, dst.Password)
			result <- err
		}()
		<-attempted
		select {
		case err := <-result:
			t.Fatalf("ticket reuse ran before CDC startup released its lock: %v", err)
		case <-time.After(40 * time.Millisecond):
		}
	}))
	select {
	case err := <-result:
		require.ErrorIs(t, err, store.ErrConnectionInUse)
	case <-time.After(time.Second):
		t.Fatal("ticket reuse did not finish after CDC registration")
	}
	current, err := store.GetConnection(dst.ID)
	require.NoError(t, err)
	require.Equal(t, dst.Database, current.Database)
}

func TestIncrementalAbortCannotPassPersistedJobBeforeRegistration(t *testing.T) {
	dst := seedDatabaseConnection(t)
	src := &store.Connection{OwnerID: 1, DBType: "mysql", Database: "source", Name: "source"}
	require.NoError(t, store.CreateConnection(src))
	job := &store.IncrementalMigrationJob{OwnerID: 1, JobID: "abort-registration-race", SrcConnID: src.ID, DstConnID: dst.ID,
		TargetSchema: "public", Status: "initializing", BootstrapDone: true}
	t.Cleanup(func() {
		cdc.Registry.Cancel(job.JobID, "abort")
		cdc.Registry.Remove(job.JobID)
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/jobs/:jobID/abort", func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("role", "user")
		AbortIncremental(c)
	})
	attempted := make(chan struct{})
	result := make(chan *httptest.ResponseRecorder, 1)
	require.NoError(t, createIncrementalAfterPreflight(job, src, dst, func() {
		go func() {
			close(attempted)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/jobs/"+job.JobID+"/abort", nil))
			result <- w
		}()
		<-attempted
		select {
		case response := <-result:
			t.Fatalf("abort ran before the new task was registered: %d %s", response.Code, response.Body.String())
		case <-time.After(40 * time.Millisecond):
		}
		_, err := cdc.Registry.Register(job.JobID)
		require.NoError(t, err)
	}))
	select {
	case response := <-result:
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	case <-time.After(time.Second):
		t.Fatal("abort did not finish after task registration")
	}
	current, err := store.GetIncrementalJob(job.JobID)
	require.NoError(t, err)
	require.Equal(t, "initializing", current.Status, "abort must cancel the registered runner, not mark an unregistered task terminal")
	require.True(t, cdc.Registry.Running(job.JobID))
	require.ErrorIs(t, store.UpdateConnection(dst.ID, map[string]any{"database": "other"}), store.ErrConnectionInUse,
		"the connection remains protected until the cancelled runner has finished")
	require.Equal(t, "abort", cdc.Registry.Remove(job.JobID))
}

func TestOscarDatabaseEditorPreservesNamesAndRejectsJDBCSeparators(t *testing.T) {
	for _, name := range []string{"业务 MiXeD DB", "percent%name", "hash#name", "name/other", "name?user=other", "name;user=other"} {
		t.Run(name, func(t *testing.T) {
			conn := seedDatabaseConnection(t)
			require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{"db_type": "oscar"}))
			payload, err := json.Marshal(map[string]string{"database": name, "expected_database": "Original"})
			require.NoError(t, err)
			called := false
			response := patchDatabase(t, conn, string(payload), func(_ context.Context, candidate *store.Connection) error {
				called = true
				require.Equal(t, "jdbc:oscar://db:5432/"+name, buildDSN(candidate))
				return nil
			})
			current, err := store.GetConnection(conn.ID)
			require.NoError(t, err)
			if strings.ContainsAny(name, "/?;") {
				require.Equal(t, http.StatusBadRequest, response.Code)
				require.False(t, called)
				require.Equal(t, "Original", current.Database)
			} else {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				require.True(t, called)
				require.Equal(t, name, current.Database)
			}
		})
	}
}

func TestConnectionCatalogsOwnershipAndManualFallback(t *testing.T) {
	conn := seedDatabaseConnection(t)
	called := false
	query := func(context.Context, *store.Connection) ([]string, error) {
		called = true
		return []string{"MiXeD", "Another DB"}, nil
	}
	handler := func(c *gin.Context) { listConnectionCatalogs(c, query) }
	response := databaseConnectionRequest(t, http.MethodGet, "/catalogs", "", conn.ID, 2, "user", handler)
	require.Equal(t, 404, response.Code)
	require.False(t, called)
	response = databaseConnectionRequest(t, http.MethodGet, "/catalogs", "", conn.ID, 1, "user", handler)
	require.Equal(t, 200, response.Code)
	require.JSONEq(t, `["MiXeD","Another DB"]`, response.Body.String())
	require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{"db_type": "oscar"}))
	called = false
	response = databaseConnectionRequest(t, http.MethodGet, "/catalogs", "", conn.ID, 1, "user", handler)
	require.Equal(t, 200, response.Code)
	require.JSONEq(t, "[]", response.Body.String())
	require.False(t, called, "Oscar does not advertise unsupported catalog enumeration")
	require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{"db_type": "postgres"}))
	response = databaseConnectionRequest(t, http.MethodGet, "/catalogs", "", conn.ID, 1, "user", func(c *gin.Context) {
		listConnectionCatalogs(c, func(context.Context, *store.Connection) ([]string, error) { return nil, errors.New("catalog denied") })
	})
	require.Equal(t, 502, response.Code)
	require.Contains(t, response.Body.String(), "手动输入")
}

func TestCatalogQueryUsesRealDatabasesAndPreservesNames(t *testing.T) {
	for _, typ := range []string{"mysql", "sqlserver", "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(typ, func(t *testing.T) {
			driverName, query := connectionCatalogQuery(typ)
			if typ == "gaussdb" {
				require.Equal(t, "opengauss", driverName)
			} else if typ == "highgo" {
				require.Equal(t, "highgo", driverName)
			}
			if typ != "mysql" && typ != "sqlserver" {
				require.Contains(t, query, "FROM pg_database")
				require.NotContains(t, query, "pg_namespace")
			}
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("MiXeD").AddRow("space DB"))
			catalogs, err := readConnectionCatalogs(context.Background(), db, query)
			require.NoError(t, err)
			require.Equal(t, []string{"MiXeD", "space DB"}, catalogs)
			mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("incomplete").RowError(0, errors.New("read failed")))
			_, err = readConnectionCatalogs(context.Background(), db, query)
			require.ErrorContains(t, err, "read failed")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDatabaseDSNPreservesNamesWithoutAddingOptions(t *testing.T) {
	require.Equal(t, "'业务　库'", pgDSNValue("业务　库"), "lib/pq treats Unicode spaces as option separators")
	name := `MiXeD DB/'\;?sslmode=require&password=changed}`
	conn := &store.Connection{Host: "2001:db8::1", Port: 5432, Database: name, Username: "user", Password: "p;@ss}' word"}
	conn.DBType = "mysql"
	mysqlConfig, err := mysql.ParseDSN(buildDSN(conn))
	require.NoError(t, err)
	require.Equal(t, name, mysqlConfig.DBName)
	require.Equal(t, conn.Password, mysqlConfig.Passwd)
	require.Equal(t, "[2001:db8::1]:5432", mysqlConfig.Addr)
	conn.DBType = "sqlserver"
	conn.Host = `db\INSTANCE`
	sqlServerConfig, err := msdsn.Parse(buildDSN(conn))
	require.NoError(t, err)
	require.Equal(t, name, sqlServerConfig.Database)
	require.Equal(t, conn.Password, sqlServerConfig.Password)
	require.Equal(t, "db", sqlServerConfig.Host)
	require.Equal(t, "INSTANCE", sqlServerConfig.Instance)
	for _, typ := range []string{"postgres", "gaussdb", "highgo", "seabox", "kingbase", "vastbase", "gbase"} {
		conn.DBType, conn.Host = typ, "localhost"
		dsn := buildDSN(conn)
		require.Contains(t, dsn, `dbname='MiXeD DB/\'\\;?sslmode=require&password=changed}' sslmode=disable`)
		_, err := pq.NewConnector(dsn)
		require.NoError(t, err)
	}
	conn.DBType = "oscar"
	conn.Database = "业务 MiXeD DB"
	require.Equal(t, "jdbc:oscar://localhost:5432/业务 MiXeD DB", buildDSN(conn),
		"Oscar JDBC does not URL-decode DBNAME; percent-encoding changes the selected database")
}
