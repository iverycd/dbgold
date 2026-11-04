package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMySQLReadStrategyOnDataRequests(t *testing.T) {
	migrationHandlerTestDB(t)
	connections := make(map[string]uint)
	for _, product := range []string{"mysql", "gaussdb", "postgres"} {
		conn := &store.Connection{OwnerID: 7, Name: product, DBType: product, Host: "127.0.0.1", Port: 1, Database: "test"}
		require.NoError(t, store.CreateConnection(conn))
		connections[product] = conn.ID
	}
	for _, tc := range []struct {
		name, source, target, strategy, content string
		objects                                 bool
		valid                                   bool
	}{
		{"default", "mysql", "gaussdb", "", "both", false, true},
		{"explicit offset", "mysql", "gaussdb", "offset", "both", false, true},
		{"stream both", "mysql", "gaussdb", "stream", "both", false, true},
		{"stream data", "mysql", "gaussdb", "stream", "data_only", false, true},
		{"non MySQL source", "postgres", "mysql", "stream", "both", false, false},
		{"other target", "mysql", "postgres", "stream", "both", false, false},
		{"schema only", "mysql", "gaussdb", "stream", "schema_only", false, false},
		{"range is experimental", "mysql", "gaussdb", "range", "data_only", false, false},
		{"objects preflight", "mysql", "gaussdb", "stream", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"src_conn_id": connections[tc.source], "dst_conn_id": connections[tc.target], "migrate_mode": "all", "migrate_content": tc.content}
			if tc.strategy != "" {
				body["mysql_read_strategy"] = tc.strategy
			}
			if tc.objects {
				body["migrate_objects"] = []string{"indexes"}
				body["table_names"] = []string{"test"}
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			call := func(handler gin.HandlerFunc) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(raw))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set("userID", uint(7))
				handler(c)
				return rec
			}
			preflight := call(PreflightDataMigration)
			if !tc.valid {
				require.Equal(t, http.StatusBadRequest, preflight.Code, preflight.Body.String())
				if !tc.objects {
					start := call(StartDataMigration)
					require.Equal(t, http.StatusBadRequest, start.Code, start.Body.String())
				}
				return
			}
			// A valid preflight reaches the deliberately closed source port.
			require.Equal(t, http.StatusBadGateway, preflight.Code, preflight.Body.String())
			start := call(StartDataMigration)
			require.Equal(t, http.StatusOK, start.Code, start.Body.String())
			var response struct {
				JobID string `json:"job_id"`
			}
			require.NoError(t, json.Unmarshal(start.Body.Bytes(), &response))
			require.NotEmpty(t, response.JobID)
			require.Eventually(t, func() bool { return datamigrate.Registry.Get(response.JobID) == nil }, 3*time.Second, 5*time.Millisecond)
			job, err := store.GetDataMigrationJobWithConn(response.JobID)
			require.NoError(t, err)
			require.Equal(t, tc.strategy, job.MySQLReadStrategy)
			require.Equal(t, "failed", job.Status)
			raw, err = json.Marshal(job)
			require.NoError(t, err)
			var detail map[string]any
			require.NoError(t, json.Unmarshal(raw, &detail))
			if tc.strategy == "" {
				require.NotContains(t, detail, "mysql_read_strategy")
			} else {
				require.Equal(t, tc.strategy, detail["mysql_read_strategy"])
			}
		})
	}
}
