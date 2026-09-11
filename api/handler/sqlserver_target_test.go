package handler

import (
	"dbgold/datamigrate/typemap"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMySQLSQLServerRegistration(t *testing.T) {
	require.True(t, isSupportedPair("mysql", "sqlserver"))
	for _, src := range []string{"postgres", "oracle", "sqlserver"} {
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
	row := batchRow{SrcDBType: "mysql", SrcHost: "source", SrcDatabase: "app", SrcUsername: "test", DstDBType: "sqlserver", DstHost: "target", DstDatabase: "app", DstUsername: "test"}
	validateBatchRow(&row, "3306", "1433")
	require.True(t, row.Supported, row.Reason)
	require.Equal(t, "dbo", row.TargetSchema)
}
