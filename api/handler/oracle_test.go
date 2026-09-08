package handler

import (
	"context"
	"net/url"
	"testing"

	"dbgold/store"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin/binding"
	"github.com/stretchr/testify/require"
)

func TestOracleTargetRegistrationAndEscapedDSN(t *testing.T) {
	connection := &connectionRequest{Name: "oracle", DBType: "oracle", Host: "localhost", Port: 1521, Database: "service", Username: "u", Password: "p"}
	require.NoError(t, binding.Validator.ValidateStruct(connection))
	dsn := buildDSN(&store.Connection{DBType: "oracle", Host: "db", Port: 1521, Database: "service", Username: "a@b", Password: "x:/?#@%"})
	u, e := url.Parse(dsn)
	require.NoError(t, e)
	require.Equal(t, "a@b", u.User.Username())
	password, _ := u.User.Password()
	require.Equal(t, "x:/?#@%", password)
	require.Equal(t, "/service", u.Path)
	require.True(t, isSupportedPair("mysql", "oracle"))
	for _, src := range []string{"postgres", "oracle", "dameng", "sqlserver"} {
		require.False(t, isSupportedPair(src, "oracle"))
	}
	require.Equal(t, "oracle", normalizeDBType("ORACLE"))
	truth := true
	require.False(t, resolveChangeOwner("oracle", &truth))
	require.False(t, (batchOptions{ChangeOwner: &truth}).changeOwnerFor("oracle"))
	require.NoError(t, binding.Validator.ValidateStruct(&submitTicketRequest{CaptchaID: "c", CaptchaCode: "c", SrcDBType: "mysql", DstDBType: "oracle"}))
}
func TestOracleSchemaListIncludesEmptySchemas(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	m.ExpectQuery("SELECT USERNAME FROM ALL_USERS").WillReturnRows(sqlmock.NewRows([]string{"USERNAME"}).AddRow("APP").AddRow("EMPTY_APP"))
	schemas, e := oracleSchemas(context.Background(), db)
	require.NoError(t, e)
	require.Equal(t, []string{"APP", "EMPTY_APP"}, schemas)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleBatchSchemaDefaultsToLoginUser(t *testing.T) {
	r := batchRow{SrcDBType: "mysql", SrcHost: "src", SrcDatabase: "db", SrcUsername: "reader", DstDBType: "oracle", DstHost: "dst", DstDatabase: "service", DstUsername: "app"}
	validateBatchRow(&r, "3306", "1521")
	require.True(t, r.Supported, r.Reason)
	require.Empty(t, r.TargetSchema)
	r.DstDBType = "postgres"
	validateBatchRow(&r, "3306", "5432")
	require.False(t, r.Supported)
	require.Contains(t, r.Reason, "target_schema")
}
