package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/api/handler"
	"dbgold/diff"
	"dbgold/store"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Uses isolated test servers and creates unique fixture scopes on each run.
func TestSchemaCompareApplyLive(t *testing.T) {
	if os.Getenv("DBGOLD_SCHEMA_COMPARE_TEST_DDL") != "1" {
		t.Skip("enable DDL against dedicated test databases with DBGOLD_SCHEMA_COMPARE_TEST_DDL=1")
	}
	setupConnTest(t)
	router := gin.New()
	router.Use(fakeAuth(1, "user"))
	router.POST("/compare", handler.CompareTableSchemas)
	router.POST("/apply", handler.ApplyTableSchemaChanges)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	password := os.Getenv("DBGOLD_SCHEMA_COMPARE_TEST_PASSWORD")
	prefix := fmt.Sprintf("sc%d", time.Now().UnixNano())
	for _, pair := range []struct{ source, target string }{
		{"mysql", "mysql"}, {"mysql", "postgres"}, {"mysql", "dameng"}, {"postgres", "postgres"},
	} {
		t.Run(pair.source+"_to_"+pair.target, func(t *testing.T) {
			open := func(product, suffix string) (*sql.DB, *store.Connection, string) {
				t.Helper()
				port, err := strconv.Atoi(os.Getenv("DBGOLD_SCHEMA_COMPARE_" + strings.ToUpper(product) + "_PORT"))
				if err != nil {
					t.Skipf("test port for %s is not set", product)
				}
				namespace := prefix + suffix
				conn := &store.Connection{OwnerID: 1, Name: namespace, Host: "127.0.0.1", Port: port, DBType: product, Password: password}
				var driver, dsn, create, cleanup string
				switch product {
				case "mysql":
					driver = "mysql"
					conn.Username, conn.Database = "root", namespace
					dsn = fmt.Sprintf("root:%s@tcp(127.0.0.1:%d)/?charset=utf8mb4", password, port)
					create = "CREATE DATABASE " + namespace + " CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci"
					cleanup = "DROP DATABASE " + namespace
				case "postgres":
					driver = "postgres"
					conn.Username, conn.Database = "postgres", "postgres"
					dsn = fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", password, port)
					create = "CREATE SCHEMA " + namespace
					cleanup = "DROP SCHEMA " + namespace + " CASCADE"
				case "dameng":
					driver = "dm"
					namespace = strings.ToUpper(namespace)
					conn.Username, conn.Database = "SYSDBA", namespace
					dsn = fmt.Sprintf("dm://SYSDBA:%s@127.0.0.1:%d", password, port)
					create = "CREATE USER " + namespace + " IDENTIFIED BY \"CompareTestUser2026\""
					cleanup = "DROP USER " + namespace + " CASCADE"
				}
				db, err := sql.Open(driver, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				_, err = db.ExecContext(ctx, create)
				require.NoError(t, err, create)
				t.Cleanup(func() {
					cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
					defer stop()
					_, err := db.ExecContext(cleanupCtx, cleanup)
					require.NoError(t, err, cleanup)
				})
				require.NoError(t, store.CreateConnection(conn))
				return db, conn, namespace
			}
			suffix := string(pair.target[0])
			src, sc, from := open(pair.source, suffix+"src")
			dst, dc, to := open(pair.target, suffix+"dst")
			execute := func(db *sql.DB, statements ...string) {
				t.Helper()
				for _, stmt := range statements {
					_, err := db.ExecContext(ctx, stmt)
					require.NoError(t, err, stmt)
				}
			}
			// Account already exists at the target. Child must be created with
			// its FK, and the source note default must remain manual.
			account := from + ".account"
			child := from + ".child"
			execute(src,
				"CREATE TABLE "+account+" (id INT PRIMARY KEY, title VARCHAR(30) DEFAULT 'source', memo VARCHAR(40))",
				"CREATE INDEX idx_memo ON "+account+" (memo)",
				"CREATE TABLE "+child+" (id INT PRIMARY KEY, account_id INT, CONSTRAINT fk_child FOREIGN KEY(account_id) REFERENCES "+account+"(id))",
				"INSERT INTO "+account+" (id,title) VALUES (1,'preserve source')",
			)
			targetAccount, targetChild := to+".account", to+".child"
			titleColumn := "title"
			// Quoting the literal in the fixture below uses each server's usual
			// session rules. The preview itself defines its own SQL literal mode.
			comment := "说明 'quoted' " + string(rune(92)) + "path"
			commentSQL := "'说明 ''quoted'' " + string(rune(92)) + "path'"
			if pair.source == "mysql" {
				commentSQL = strings.ReplaceAll(commentSQL, string(rune(92)), string(rune(92))+string(rune(92)))
				execute(src,
					"ALTER TABLE "+account+" COMMENT = "+commentSQL,
					"ALTER TABLE "+account+" MODIFY COLUMN title VARCHAR(30) DEFAULT 'source' COMMENT "+commentSQL,
				)
			} else {
				execute(src,
					"COMMENT ON TABLE "+account+" IS "+commentSQL,
					"COMMENT ON COLUMN "+account+".title IS "+commentSQL,
				)
			}
			if pair.target == "dameng" {
				targetAccount, targetChild = to+".ACCOUNT", to+".CHILD"
				titleColumn = "TITLE"
			}
			execute(dst,
				"CREATE TABLE "+targetAccount+" (id INT PRIMARY KEY, "+titleColumn+" VARCHAR(30) DEFAULT 'target')",
				"INSERT INTO "+targetAccount+" (id,"+titleColumn+") VALUES (2,'preserve target')",
			)
			request := map[string]any{
				"source": map[string]any{"connection_id": sc.ID, "namespace": from},
				"target": map[string]any{"connection_id": dc.ID, "namespace": to},
			}
			post := func(path string, value any, status int, result any) {
				t.Helper()
				body, err := json.Marshal(value)
				require.NoError(t, err)
				out := httptest.NewRecorder()
				router.ServeHTTP(out, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
				require.Equal(t, status, out.Code, "%s: %s", path, out.Body.String())
				if result != nil {
					require.NoError(t, json.Unmarshal(out.Body.Bytes(), result))
				}
			}
			type preview struct {
				PlanID  string        `json:"plan_id"`
				Changes []diff.Change `json:"changes"`
			}
			var initial preview
			post("/compare", request, http.StatusOK, &initial)
			require.NotEmpty(t, initial.PlanID)
			ids := []string{}
			for _, change := range initial.Changes {
				if change.Status == diff.Applicable && change.Kind != "column_missing" && change.Kind != "index" {
					ids = append(ids, change.ID)
				}
				if change.Kind == "column_comment" {
					require.Equal(t, comment, change.Expected)
				}
			}
			require.GreaterOrEqual(t, len(ids), 4, "%+v", initial.Changes)
			var applied struct {
				Results []struct{ Status, Error, Kind string }
			}
			// Drift after preview must prevent all requested DDL until the
			// user sees an up-to-date comparison.
			execute(dst, "CREATE TABLE "+to+".extra_target (sentinel INT)")
			post("/apply", map[string]any{"plan_id": initial.PlanID, "change_ids": ids}, http.StatusConflict, nil)
			post("/compare", request, http.StatusOK, &initial)
			post("/apply", map[string]any{"plan_id": initial.PlanID, "change_ids": ids}, http.StatusOK, &applied)
			for _, result := range applied.Results {
				require.Equal(t, "applied", result.Status, "%s: %s", result.Kind, result.Error)
			}
			require.Len(t, applied.Results, len(ids))
			post("/apply", map[string]any{"plan_id": initial.PlanID, "change_ids": ids}, http.StatusConflict, nil)
			var refreshed preview
			post("/compare", request, http.StatusOK, &refreshed)
			kinds := map[string]bool{}
			for _, change := range refreshed.Changes {
				require.NotContains(t, ids, change.ID, "selected difference should have disappeared: %+v", change)
				kinds[change.Kind] = true
			}
			for _, kind := range []string{"column_definition", "column_missing", "index"} {
				require.True(t, kinds[kind], "%s: %+v", kind, refreshed.Changes)
			}
			var additive []string
			for _, change := range refreshed.Changes {
				if change.Status == diff.Applicable && (change.Kind == "column_missing" || change.Kind == "index") {
					additive = append(additive, change.ID)
				}
			}
			require.GreaterOrEqual(t, len(additive), 2, "%+v", refreshed.Changes)
			applied.Results = nil
			post("/apply", map[string]any{"plan_id": refreshed.PlanID, "change_ids": additive}, http.StatusOK, &applied)
			for _, result := range applied.Results {
				require.Equal(t, "applied", result.Status, "%s: %s", result.Kind, result.Error)
			}
			require.Len(t, applied.Results, len(additive))
			var last preview
			post("/compare", request, http.StatusOK, &last)
			for _, change := range last.Changes {
				require.Equal(t, diff.Manual, change.Status, "%+v", change)
			}
			var memo sql.NullString
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT memo FROM "+targetAccount).Scan(&memo))
			require.False(t, memo.Valid)
			for _, check := range []struct {
				db                   *sql.DB
				table, column, value string
			}{{src, account, "title", "preserve source"}, {dst, targetAccount, titleColumn, "preserve target"}} {
				var value string
				require.NoError(t, check.db.QueryRowContext(ctx, "SELECT "+check.column+" FROM "+check.table).Scan(&value))
				require.Equal(t, check.value, value)
			}
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+targetChild).Scan(&count))
			require.Zero(t, count)
		})
	}
}
