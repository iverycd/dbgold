package handler

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"dbgold/store"
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
	srcTicket := &submitTicketRequest{
		CaptchaID: "captcha", CaptchaCode: "code", SrcDBType: "oscar", DstDBType: "mysql",
	}
	require.NoError(t, binding.Validator.ValidateStruct(srcTicket))
	require.NoError(t, binding.Validator.ValidateStruct(&updateTicketInfoRequest{SrcDBType: "oscar", DstDBType: "mysql"}))

	for _, src := range []string{"mysql", "sqlserver", "oracle", "dameng"} {
		if !isSupportedPair(src, "oscar") {
			t.Errorf("missing migration pair %s -> oscar", src)
		}
	}
	require.True(t, isSupportedPair("oscar", "mysql"))
	require.False(t, isSupportedPair("oscar", "postgres"))
	owner, distributed := resolveOwnerAndDistribution("oscar", "mysql", true, true)
	require.False(t, owner)
	require.False(t, distributed)
	if normalizeDBType("ShenTong") != "oscar" || normalizeDBType("OSCAR") != "oscar" {
		t.Fatal("Oscar batch aliases are not registered")
	}
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
