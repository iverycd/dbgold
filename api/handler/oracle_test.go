package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"dbgold/store"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
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
	m.ExpectQuery("SELECT USERNAME FROM ALL_USERS").WillReturnRows(sqlmock.NewRows([]string{"USERNAME"}).AddRow("APP").AddRow("EMPTY_APP").AddRow("MixedCase"))
	schemas, e := oracleSchemas(context.Background(), db)
	require.NoError(t, e)
	require.Equal(t, []string{"APP", "EMPTY_APP", "MixedCase"}, schemas)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleSchemaListReturnsQueryAndFetchErrors(t *testing.T) {
	for _, phase := range []string{"query", "fetch", "scan"} {
		t.Run(phase, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			query := mock.ExpectQuery("SELECT USERNAME FROM ALL_USERS")
			failure := errors.New("ORA-01031: insufficient privileges")
			switch phase {
			case "query":
				query.WillReturnError(failure)
			case "fetch":
				query.WillReturnRows(sqlmock.NewRows([]string{"USERNAME"}).AddRow("APP").AddRow("OTHER").RowError(1, failure))
			case "scan":
				query.WillReturnRows(sqlmock.NewRows([]string{"USERNAME"}).AddRow(nil))
			}
			schemas, err := oracleSchemas(context.Background(), db)
			require.Error(t, err)
			require.Nil(t, schemas, "failed queries must not return a partial schema list")
			if phase != "scan" {
				require.ErrorIs(t, err, failure)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOracleSchemaListHonorsQueryDeadline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT USERNAME FROM ALL_USERS").WillDelayFor(time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"USERNAME"}).AddRow("APP"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	schemas, err := oracleSchemas(ctx, db)
	require.Error(t, err)
	require.Nil(t, schemas)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOracleSchemaEndpointsHonorRequestContext(t *testing.T) {
	conn := seedDatabaseConnection(t)
	require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{
		"db_type": "oracle", "host": "127.0.0.1", "port": 1, "database": "ORCLPDB1",
	}))
	for _, endpoint := range []struct {
		name    string
		handler gin.HandlerFunc
	}{
		{"source databases", ListConnectionDatabases},
		{"target schemas", ListConnectionSchemas},
	} {
		for _, expired := range []bool{true, false} {
			t.Run(endpoint.name+"/expired="+strconv.FormatBool(expired), func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				status, message := http.StatusBadGateway, "context canceled"
				if expired {
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					status, message = http.StatusGatewayTimeout, "读取 Oracle Schema 列表超时"
				} else {
					ctx, cancel = context.WithCancel(context.Background())
					cancel()
				}
				defer cancel()
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(conn.ID), 10)}}
				c.Set("userID", uint(1))
				c.Set("role", "user")
				c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
				endpoint.handler(c)
				require.Equal(t, status, response.Code, response.Body.String())
				require.Contains(t, response.Body.String(), message)
				require.NotContains(t, response.Body.String(), conn.Password)
				require.Len(t, c.Errors, 1)
			})
		}
	}
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
