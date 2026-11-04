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

func TestMySQLCopyStrategiesOnDataRequests(t *testing.T) {
	migrationHandlerTestDB(t)
	connections := make(map[string]uint)
	for _, product := range []string{"mysql", "gaussdb", "postgres", "dameng"} {
		conn := &store.Connection{OwnerID: 7, Name: product, DBType: product, Host: "127.0.0.1", Port: 1, Database: "test"}
		require.NoError(t, store.CreateConnection(conn))
		connections[product] = conn.ID
	}
	for _, tc := range []struct {
		name, source, target, option, strategy, content string
		objects, valid                                  bool
		want                                            string
	}{
		{"default", "mysql", "gaussdb", "mysql_read_strategy", "", "both", false, true, ""},
		{"explicit offset", "mysql", "gaussdb", "mysql_read_strategy", "offset", "both", false, true, "offset"},
		{"stream both", "mysql", "gaussdb", "mysql_read_strategy", "stream", "both", false, true, "stream"},
		{"stream data", "mysql", "gaussdb", "mysql_read_strategy", "stream", "data_only", false, true, "stream"},
		{"non MySQL source", "postgres", "mysql", "mysql_read_strategy", "stream", "both", false, false, ""},
		{"other target", "mysql", "postgres", "mysql_read_strategy", "stream", "both", false, false, ""},
		{"schema only", "mysql", "gaussdb", "mysql_read_strategy", "stream", "schema_only", false, false, ""},
		{"range is experimental", "mysql", "gaussdb", "mysql_read_strategy", "range", "data_only", false, false, ""},
		{"objects preflight", "mysql", "gaussdb", "mysql_read_strategy", "stream", "", true, false, ""},
		{"default DM", "mysql", "dameng", "dameng_write_strategy", "", "both", false, true, "batch"},
		{"DM implicit content", "mysql", "dameng", "dameng_write_strategy", "", "", false, true, "batch"},
		{"DM data only", "mysql", "dameng", "dameng_write_strategy", "", "data_only", false, true, "batch"},
		{"DM schema only default", "mysql", "dameng", "dameng_write_strategy", "", "schema_only", false, true, ""},
		{"other pair default", "mysql", "postgres", "dameng_write_strategy", "", "both", false, true, ""},
		{"DM row", "mysql", "dameng", "dameng_write_strategy", "row", "both", false, true, "row"},
		{"DM batch", "mysql", "dameng", "dameng_write_strategy", "batch", "data_only", false, true, "batch"},
		{"DM batch other source", "postgres", "dameng", "dameng_write_strategy", "batch", "both", false, false, ""},
		{"DM batch other target", "mysql", "gaussdb", "dameng_write_strategy", "batch", "both", false, false, ""},
		{"DM unknown write", "mysql", "dameng", "dameng_write_strategy", "bulk", "both", false, false, ""},
		{"DM batch schema only", "mysql", "dameng", "dameng_write_strategy", "batch", "schema_only", false, false, ""},
		{"DM batch objects", "mysql", "dameng", "dameng_write_strategy", "batch", "", true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"src_conn_id": connections[tc.source], "dst_conn_id": connections[tc.target], "migrate_mode": "all", "migrate_content": tc.content}
			if tc.strategy != "" {
				body[tc.option] = tc.strategy
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
			if tc.option == "dameng_write_strategy" {
				require.Equal(t, tc.want, job.DamengWriteStrategy)
			} else {
				require.Equal(t, tc.want, job.MySQLReadStrategy)
			}
			require.Equal(t, "failed", job.Status)
			raw, err = json.Marshal(job)
			require.NoError(t, err)
			var detail map[string]any
			require.NoError(t, json.Unmarshal(raw, &detail))
			if tc.want == "" {
				require.NotContains(t, detail, tc.option)
			} else {
				require.Equal(t, tc.want, detail[tc.option])
			}
		})
	}
}

func TestDamengWriteStrategyOnViewPreflight(t *testing.T) {
	migrationHandlerTestDB(t)
	src := &store.Connection{OwnerID: 7, Name: "src", DBType: "mysql", Host: "127.0.0.1", Port: 1}
	dst := &store.Connection{OwnerID: 7, Name: "dst", DBType: "dameng", Host: "127.0.0.1", Port: 1}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	body := map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "migrate_mode": "all", "view_names": []string{"V"}}
	for _, policy := range []string{"", "row", "batch"} {
		body["dameng_write_strategy"] = policy
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("userID", uint(7))
		PreflightDataMigration(c)
		if policy == "" {
			require.Equal(t, http.StatusBadGateway, rec.Code, "the closed source port was reached")
		} else {
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "包含表数据")
		}
	}
}

func TestDamengWriteStrategyDoesNotChangeSavedJobs(t *testing.T) {
	migrationHandlerTestDB(t)
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{JobID: "legacy", SrcDBType: "mysql", DstDBType: "dameng", RepairConfigJSON: `{"Content":"both"}`}))
	job, err := store.GetDataMigrationJobWithConn("legacy")
	require.NoError(t, err)
	require.Empty(t, job.DamengWriteStrategy)
	cfg, err := recoveryConfig(&store.MigrationRepairBinding{Configured: true, ConfigJSON: job.RepairConfigJSON})
	require.NoError(t, err)
	require.Empty(t, cfg.DamengWriteStrategy)
}
