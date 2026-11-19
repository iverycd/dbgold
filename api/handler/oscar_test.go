package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/stretchr/testify/require"
)

func TestOscarValidationDSNAndMigrationPairs(t *testing.T) {
	connection := &connectionRequest{
		Name: "oscar", DBType: "oscar", Host: "127.0.0.1", Port: 2003,
		Database: "OSRDB", Username: "user", Password: "secret",
	}
	if err := binding.Validator.ValidateStruct(connection); err != nil {
		t.Fatalf("Oscar connection request rejected: %v", err)
	}
	dsn := buildDSN(&store.Connection{DBType: "oscar", Host: "db", Port: 2003, Database: "OSRDB", Username: "user", Password: "secret"})
	if dsn != "jdbc:oscar://db:2003/OSRDB" {
		t.Fatalf("Oscar URL = %q", dsn)
	}
	if strings.Contains(dsn, "user") || strings.Contains(dsn, "secret") {
		t.Fatalf("Oscar URL leaks credentials: %q", dsn)
	}

	targetOnly := &submitTicketRequest{
		CaptchaID: "captcha", CaptchaCode: "code", SrcDBType: "mysql", DstDBType: "oscar",
	}
	if err := binding.Validator.ValidateStruct(targetOnly); err != nil {
		t.Fatalf("Oscar ticket target rejected: %v", err)
	}
	for _, dst := range []string{"mysql", "dameng"} {
		srcTicket := &submitTicketRequest{
			CaptchaID: "captcha", CaptchaCode: "code", SrcDBType: "oscar", DstDBType: dst,
		}
		require.NoError(t, binding.Validator.ValidateStruct(srcTicket))
		require.NoError(t, binding.Validator.ValidateStruct(&updateTicketInfoRequest{SrcDBType: "oscar", DstDBType: dst}))
		require.True(t, isSupportedPair("oscar", dst))
		owner, distributed := resolveOwnerAndDistribution("oscar", dst, true, true)
		require.False(t, owner)
		require.False(t, distributed)
	}

	for _, src := range []string{"mysql", "sqlserver", "oracle", "dameng"} {
		if !isSupportedPair(src, "oscar") {
			t.Errorf("missing migration pair %s -> oscar", src)
		}
	}
	require.False(t, isSupportedPair("oscar", "postgres"))
	if normalizeDBType("ShenTong") != "oscar" || normalizeDBType("OSCAR") != "oscar" {
		t.Fatal("Oscar batch aliases are not registered")
	}
}

func TestOscarDamengViewMigrationCapabilities(t *testing.T) {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	GetSupportedPairs(c)
	var pairs []SupportedPair
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &pairs))
	actual := map[string]bool{}
	for _, p := range pairs {
		if p.Source == "oscar" && (p.Target == "mysql" || p.Target == "dameng") {
			require.NotContains(t, actual, p.Target)
			actual[p.Target] = p.SupportsViewMigration
		}
	}
	require.Equal(t, map[string]bool{"mysql": true, "dameng": false}, actual)

	migrationHandlerTestDB(t)
	src := &store.Connection{OwnerID: 7, Name: "src", DBType: "oscar"}
	dst := &store.Connection{OwnerID: 7, Name: "dst", DBType: "dameng"}
	require.NoError(t, store.CreateConnection(src))
	require.NoError(t, store.CreateConnection(dst))
	body, err := json.Marshal(map[string]any{"src_conn_id": src.ID, "dst_conn_id": dst.ID, "migrate_mode": "all", "view_names": []string{"V"}})
	require.NoError(t, err)
	for _, h := range []gin.HandlerFunc{MigrateViews, PreflightDataMigration} {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Set("userID", uint(7))
		c.Set("role", "user")
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		h(c)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.Contains(t, response.Body.String(), "暂不支持单独迁移视图")
	}
	var count int64
	require.NoError(t, store.DB.Model(&store.DataMigrationJob{}).Count(&count).Error)
	require.Zero(t, count)
}

type oscarSchemaReader struct {
	source.Reader
	schema string
}

func (r oscarSchemaReader) MigrationSchema() string { return r.schema }

func TestOscarSourceConnectionAndRepairScopeKeepDatabaseAndSchemaSeparate(t *testing.T) {
	migrationHandlerTestDB(t)
	conn := &store.Connection{DBType: "oscar", Name: "oscar", Host: "db", Port: 2003, Database: "OSRDB", Username: "user"}
	const schema = "MixedSchema"
	job := &store.DataMigrationJob{
		JobID: "oscar-source-scope", SrcDBType: "oscar", DstDBType: "mysql", SrcConnName: conn.Name,
		SrcConnHost: conn.Host, SrcConnPort: conn.Port, SrcConnUsername: conn.Username,
		SrcConnDatabase: migrationSourceDisplayDatabase(conn, schema),
	}
	// Direct API and Excel rows may omit the scope. Record the reader's actual schema.
	resolved := resolvedMigrationSourceScope(oscarSchemaReader{schema: schema}, conn.DBType, "mysql", "")
	migrationSourceLocation(job, conn, resolved)
	require.NoError(t, store.CreateDataMigrationJob(job))
	saved, err := store.GetDataMigrationJobWithConn(job.JobID)
	require.NoError(t, err)
	require.Equal(t, "OSRDB", saved.SrcConn.Database)
	require.Equal(t, "OSRDB", saved.SourceDatabase)
	require.Equal(t, "OSRDB", saved.SourceConnectionDatabase)
	require.Equal(t, schema, saved.SourceSchema)
	binding := initialRepairBinding(&saved.DataMigrationJob, "{}", false)
	require.Equal(t, schema, sourceOverride(binding, "oscar"))
	require.NoError(t, checkRepairConnection(&saved.DataMigrationJob, conn, true))
	conn.Database = schema
	require.ErrorContains(t, checkRepairConnection(&saved.DataMigrationJob, conn, true), "数据库")
}
