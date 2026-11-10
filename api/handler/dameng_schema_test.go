package handler

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"dbgold/store"

	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	testDamengSchemaCatalog = `SELECT NAME FROM SYSOBJECTS WHERE TYPE$='SCH' ORDER BY NAME`
	testDamengSchemaOwners  = `SELECT DISTINCT OWNER FROM ALL_TABLES ORDER BY OWNER`
)

func TestDamengSourceSchemaCatalog(t *testing.T) {
	for _, names := range [][]string{{"APP", "EmptySchema", "MixedCase", "SYSDBA"}, {}} {
		t.Run(strconv.Itoa(len(names))+" schemas", func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			defer db.Close()
			rows := sqlmock.NewRows([]string{"NAME"})
			for _, name := range names {
				rows.AddRow(name)
			}
			mock.ExpectQuery(testDamengSchemaCatalog).WillReturnRows(rows).RowsWillBeClosed()

			schemas, err := damengSourceSchemas(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, names, schemas)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDamengSourceSchemaCatalogFallback(t *testing.T) {
	for _, code := range []int32{-2106, -5504, -5508} {
		t.Run(strconv.Itoa(int(code)), func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery(testDamengSchemaCatalog).WillReturnError(&dm.DmError{ErrCode: code})
			mock.ExpectQuery(testDamengSchemaOwners).
				WillReturnRows(sqlmock.NewRows([]string{"OWNER"}).AddRow("MixedOwner")).RowsWillBeClosed()

			schemas, err := damengSourceSchemas(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, []string{"MixedOwner"}, schemas)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDamengSourceSchemaCatalogErrors(t *testing.T) {
	for _, phase := range []string{"query", "fetch", "scan", "fallback"} {
		t.Run(phase, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			defer db.Close()
			query := mock.ExpectQuery(testDamengSchemaCatalog)
			failure := errors.New("schema query failed")
			switch phase {
			case "query":
				failure = &dm.DmError{ErrCode: -2007}
				query.WillReturnError(failure)
			case "fetch":
				// A permission error during iteration must also discard a partial list.
				failure = &dm.DmError{ErrCode: -5504}
				rows := sqlmock.NewRows([]string{"NAME"}).AddRow("APP").AddRow("OTHER").RowError(1, failure)
				query.WillReturnRows(rows).RowsWillBeClosed()
			case "scan":
				query.WillReturnRows(sqlmock.NewRows([]string{"NAME"}).AddRow("APP").AddRow(nil)).RowsWillBeClosed()
			case "fallback":
				query.WillReturnError(&dm.DmError{ErrCode: -5504})
				mock.ExpectQuery(testDamengSchemaOwners).WillReturnError(failure)
			}

			schemas, err := damengSourceSchemas(context.Background(), db)
			require.Error(t, err)
			require.Nil(t, schemas)
			if phase != "scan" {
				require.ErrorIs(t, err, failure)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDamengSourceSchemaEndpointHandshake(t *testing.T) {
	conn := seedDatabaseConnection(t)
	for _, tc := range []struct {
		name    string
		status  int
		message string
	}{
		{"deadline", http.StatusGatewayTimeout, "读取达梦源 Schema 列表超时"},
		{"canceled", http.StatusBadGateway, "读取达梦源 Schema 列表失败"},
		{"disconnected", http.StatusBadGateway, "读取达梦源 Schema 列表失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			defer listener.Close()
			require.NoError(t, listener.SetDeadline(time.Now().Add(3*time.Second)))
			require.NoError(t, store.UpdateConnection(conn.ID, map[string]any{
				"db_type": "dameng", "host": "127.0.0.1", "port": listener.Addr().(*net.TCPAddr).Port,
			}))

			var ctx context.Context
			var cancel context.CancelFunc
			if tc.name == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(conn.ID), 10)}}
			c.Set("userID", uint(1))
			c.Set("role", "user")
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				ListConnectionDatabases(c)
			}()

			client, err := listener.Accept()
			require.NoError(t, err)
			defer client.Close()
			require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
			// Accept a real DM handshake and leave the client waiting for a reply.
			var header [1]byte
			_, err = io.ReadFull(client, header[:])
			require.NoError(t, err)
			switch tc.name {
			case "canceled":
				cancel()
			case "disconnected":
				require.NoError(t, listener.Close())
				require.NoError(t, client.Close())
			}

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("达梦 Schema 接口未在请求截止或取消后返回")
			}
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), tc.message)
			require.NotContains(t, response.Body.String(), conn.Password)
			if tc.name != "disconnected" {
				_, err = io.Copy(io.Discard, client)
				require.NoError(t, err, "server should observe the client closing its socket")
			}
		})
	}
}
