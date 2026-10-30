package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/config"
	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

func TestPGMySQLPairsAndExclusionsPersist(t *testing.T) {
	for _, src := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		require.True(t, isSupportedPair(src, "mysql"))
	}
	store.Init(&config.Config{SQLitePath: ":memory:"})
	refs := []datamigrate.ObjectRef{{Kind: "index", Table: "Mixed", Name: "Expr"}}
	job := &store.DataMigrationJob{JobID: "pg-exclusions", ExcludedObjects: encodeExcludedObjects(refs)}
	require.NoError(t, store.CreateDataMigrationJob(job))
	saved, e := store.GetDataMigrationJob(job.JobID)
	require.NoError(t, e)
	var actual []datamigrate.ObjectRef
	require.NoError(t, json.Unmarshal([]byte(saved.ExcludedObjects), &actual))
	require.Equal(t, refs, actual)
}
func TestPGPreflightChecksConnectionOwnershipBeforeConnecting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store.Init(&config.Config{SQLitePath: ":memory:"})
	src := &store.Connection{OwnerID: 2, Name: "private", DBType: "postgres", Host: "invalid.invalid", Port: 5432}
	require.NoError(t, store.CreateConnection(src))
	router := gin.New()
	router.POST("/migration/data/preflight", func(c *gin.Context) { c.Set("userID", uint(1)); c.Set("role", "user"); PreflightDataMigration(c) })
	body, _ := json.Marshal(map[string]any{"src_conn_id": src.ID, "dst_conn_id": 99, "migrate_mode": "all"})
	req := httptest.NewRequest(http.MethodPost, "/migration/data/preflight", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGenericPartitionPreflightAcceptsSupportedPairWithoutCreatingJob(t *testing.T) {
	migrationHandlerTestDB(t)
	src := &store.Connection{OwnerID: 7, Name: "isolated-source", DBType: "mysql", Host: "127.0.0.1", Port: 1, Database: "dbgold_partition_test"}
	dst := &store.Connection{OwnerID: 7, Name: "isolated-target", DBType: "postgres", Host: "127.0.0.1", Port: 1, Database: "dbgold_partition_test"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	body, err := json.Marshal(map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "migrate_mode": "all", "target_schema": "public"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set("userID", uint(7))
	c.Set("role", "user")
	c.Request = httptest.NewRequest(http.MethodPost, "/migration/data/preflight", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	PreflightDataMigration(c)
	// A supported non-PG/MySQL pair now reaches read-only connection setup. The
	// deliberately closed loopback port cannot create or modify a database.
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "此预检仅支持")
	var jobs int64
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&jobs).Error)
	require.Zero(t, jobs)
}

func TestBatchOptionalPGSchemaDoesNotReadFirstColumn(t *testing.T) {
	f := excelize.NewFile()
	defer f.Close()
	headers := append([]string{}, batchHeaders...)
	values := []string{"postgres", "127.0.0.1", "5432", "app", "reader", "pw", "mysql", "127.0.0.1", "3306", "target", "writer", "pw", "target"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		require.NoError(t, f.SetCellValue("Sheet1", cell, h))
		cell, _ = excelize.CoordinatesToCellName(i+1, 2)
		require.NoError(t, f.SetCellValue("Sheet1", cell, values[i]))
	}
	rows, e := parseBatchExcel(f)
	require.NoError(t, e)
	require.Empty(t, rows[0].SrcSchema)
	cell, _ := excelize.CoordinatesToCellName(len(headers)+1, 1)
	require.NoError(t, f.SetCellValue("Sheet1", cell, "source_schema"))
	cell, _ = excelize.CoordinatesToCellName(len(headers)+1, 2)
	require.NoError(t, f.SetCellValue("Sheet1", cell, "MixedSchema"))
	rows, e = parseBatchExcel(f)
	require.NoError(t, e)
	require.Equal(t, "MixedSchema", rows[0].SrcSchema)
}
