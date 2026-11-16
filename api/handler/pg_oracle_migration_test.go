package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPGOracleSupportedPairsAndCheckCapability(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	GetSupportedPairs(c)
	var pairs []SupportedPair
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pairs))
	for _, src := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(src, func(t *testing.T) {
			require.True(t, isSupportedPair(src, "oracle"))
			require.NoError(t, validateCheckConstraintMigration(src, "oracle", []string{"check_constraints"}))
			found := false
			for _, pair := range pairs {
				if pair.Source == src && pair.Target == "oracle" {
					found = true
					require.True(t, pair.SupportsCheckConstraints)
					require.False(t, pair.SupportsTriggers, "business triggers remain an original-DDL export")
				}
			}
			require.True(t, found)
			row := batchRow{SrcDBType: src, SrcHost: "source", SrcDatabase: "app", SrcUsername: "reader", DstDBType: "oracle", DstHost: "target", DstDatabase: "ORCL", DstUsername: "writer"}
			validateBatchRow(&row, "5432", "1521")
			require.True(t, row.Supported, row.Reason)
		})
	}
	require.ErrorContains(t, validateCheckConstraintMigration("postgres", "mysql", []string{"check_constraints"}), "CHECK")
	require.ErrorContains(t, validateCheckConstraintMigration("mysql", "oracle", []string{"check_constraints"}), "CHECK")
	require.NoError(t, validateCheckConstraintMigration("mysql", "oracle", []string{"indexes"}))
}

func TestCheckConstraintRequestBinding(t *testing.T) {
	for _, objectMode := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"src_conn_id":1,"dst_conn_id":2,"migrate_mode":"all","migrate_objects":["check_constraints"],"table_names":["Mixed"]}`))
		c.Request.Header.Set("Content-Type", "application/json")
		if objectMode {
			var req startObjectMigrationRequest
			require.NoError(t, c.ShouldBindJSON(&req))
			require.Equal(t, []string{"check_constraints"}, req.MigrateObjects)
		} else {
			var req preflightDataMigrationRequest
			require.NoError(t, c.ShouldBindJSON(&req))
			require.Equal(t, []string{"check_constraints"}, req.MigrateObjects)
		}
	}
}

func TestPGOracleCheckRequestRejectsUnsupportedPairBeforeConnecting(t *testing.T) {
	migrationHandlerTestDB(t)
	src := &store.Connection{OwnerID: 7, Name: "source", DBType: "postgres", Host: "invalid.invalid", Port: 5432}
	dst := &store.Connection{OwnerID: 7, Name: "target", DBType: "mysql", Host: "invalid.invalid", Port: 3306}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	for _, handler := range []gin.HandlerFunc{PreflightDataMigration, StartObjectMigration} {
		body, err := json.Marshal(map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "migrate_mode": "all", "migrate_objects": []string{"check_constraints"}, "table_names": []string{"Mixed"}})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set("userID", uint(7))
		c.Set("role", "user")
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		handler(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "CHECK")
	}
	var jobs int64
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&jobs).Error)
	require.Zero(t, jobs)
}
