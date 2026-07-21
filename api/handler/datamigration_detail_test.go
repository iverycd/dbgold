package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dbgold/config"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGetDataMigrationJobDetailReturnsSnapshotsAndEnforcesOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store.Init(&config.Config{SQLitePath: ":memory:"})
	src := &store.Connection{OwnerID: 1, Name: "source", DBType: "mysql", Host: "2001:db8::1", Port: 3306, Database: "source_db", Username: "reader", Password: "secret"}
	dst := &store.Connection{OwnerID: 1, Name: "target", DBType: "postgres", Host: "10.0.0.2", Port: 5432, Database: "target_db", Username: "writer", Password: "secret"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	require.NoError(t, store.CreateDataMigrationJob(&store.DataMigrationJob{
		OwnerID: 1, JobID: "data-detail-owned", SrcConnID: src.ID, DstConnID: dst.ID,
		SrcDBType: "mysql", DstDBType: "postgres", Status: "done",
	}))

	request := func(ownerID uint) *httptest.ResponseRecorder {
		router := gin.New()
		router.GET("/migration/data-migrate/jobs/:jobID", func(c *gin.Context) {
			c.Set("userID", ownerID)
			c.Set("role", "user")
			GetDataMigrationJobDetail(c)
		})
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/migration/data-migrate/jobs/data-detail-owned", nil)
		router.ServeHTTP(recorder, req)
		return recorder
	}

	owned := request(1)
	require.Equal(t, http.StatusOK, owned.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(owned.Body.Bytes(), &response))
	require.Equal(t, "source", response["src_conn"].(map[string]any)["name"])
	require.Equal(t, "target", response["dst_conn"].(map[string]any)["name"])

	forbidden := request(2)
	require.Equal(t, http.StatusNotFound, forbidden.Code)
}
