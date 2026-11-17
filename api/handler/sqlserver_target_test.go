package handler

import (
	"dbgold/datamigrate/typemap"
	"dbgold/store"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOracleSQLServerKeepsServiceAndExactOwnerSeparate(t *testing.T) {
	conn := &store.Connection{DBType: "oracle", Database: "serviceName"}
	job := &store.DataMigrationJob{DstDBType: "sqlserver"}
	migrationSourceLocation(job, conn, "QuotedOwner")
	require.Equal(t, "serviceName", job.SourceDatabase)
	require.Equal(t, "serviceName", job.SourceConnectionDatabase)
	require.Equal(t, "QuotedOwner", job.SourceSchema)
	migrationSourceLocation(job, conn, "")
	require.Empty(t, job.SourceSchema)
}

func TestMySQLSQLServerRegistration(t *testing.T) {
	require.True(t, isSupportedPair("mysql", "sqlserver"))
	for _, src := range []string{"dameng", "oracle", "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		require.True(t, isSupportedPair(src, "sqlserver"), src)
		_, ok := typemap.Get(src, "sqlserver")
		require.True(t, ok, src)
	}
	for _, src := range []string{"sqlserver", "oscar"} {
		require.False(t, isSupportedPair(src, "sqlserver"))
	}
	_, ok := typemap.Get("mysql", "sqlserver")
	require.True(t, ok)
	require.Equal(t, "sqlserver", dbTypeAlias["mssql"])
	require.False(t, resolveChangeOwner("sqlserver", nil))
	b := true
	require.False(t, (batchOptions{ChangeOwner: &b}).changeOwnerFor("sqlserver"))
}

func TestSQLServerBatchDefaultsSchema(t *testing.T) {
	for _, src := range []string{"mysql", "oracle", "postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		row := batchRow{SrcDBType: src, SrcHost: "source", SrcDatabase: "app", SrcUsername: "test", DstDBType: "sqlserver", DstHost: "target", DstDatabase: "app", DstUsername: "test"}
		validateBatchRow(&row, "3306", "1433")
		require.True(t, row.Supported, row.Reason)
		require.Equal(t, "dbo", row.TargetSchema)
	}
}
