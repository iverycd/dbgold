package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbgold/api/handler"
	"dbgold/datamigrate/source"
	"dbgold/diff"
	"dbgold/store"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Uses isolated test servers and creates unique fixture scopes on each run.
// PG-compatible vendor fixtures take DBGOLD_SCHEMA_COMPARE_<PRODUCT>_{PORT,USER,DATABASE}
// and optional HOST (defaults to loopback) and PASSWORD (defaults to the common test password).
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
		{"mysql", "highgo"}, {"highgo", "highgo"},
		{"mysql", "gbase"}, {"gbase", "gbase"},
		{"mysql", "seabox"}, {"seabox", "seabox"},
		{"mysql", "kingbase"}, {"kingbase", "kingbase"},
	} {
		t.Run(pair.source+"_to_"+pair.target, func(t *testing.T) {
			for _, product := range []string{pair.source, pair.target} {
				if os.Getenv("DBGOLD_SCHEMA_COMPARE_"+strings.ToUpper(product)+"_PORT") == "" {
					t.Skipf("test port for %s is not set", product)
				}
			}
			open := func(product, suffix string) (*sql.DB, *store.Connection, string) {
				t.Helper()
				envPrefix := "DBGOLD_SCHEMA_COMPARE_" + strings.ToUpper(product) + "_"
				port, err := strconv.Atoi(os.Getenv(envPrefix + "PORT"))
				require.NoError(t, err, "%sPORT", envPrefix)
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
				case "dameng":
					driver = "dm"
					namespace = strings.ToUpper(namespace)
					conn.Username, conn.Database = "SYSDBA", namespace
					dsn = fmt.Sprintf("dm://SYSDBA:%s@127.0.0.1:%d", password, port)
					create = "CREATE USER " + namespace + " IDENTIFIED BY \"CompareTestUser2026\""
					cleanup = "DROP USER " + namespace + " CASCADE"
				default:
					require.True(t, source.IsPGFamily(product), "unsupported fixture product %s", product)
					driver = "postgres"
					conn.Username, conn.Database = "postgres", "postgres"
					if product != "postgres" {
						if product == "highgo" {
							driver = "highgo"
						}
						conn.Username, conn.Database = os.Getenv(envPrefix+"USER"), os.Getenv(envPrefix+"DATABASE")
						require.NotEmpty(t, conn.Username, "%sUSER", envPrefix)
						require.NotEmpty(t, conn.Database, "%sDATABASE", envPrefix)
						if host := os.Getenv(envPrefix + "HOST"); host != "" {
							conn.Host = host
						}
						if pass, supplied := os.LookupEnv(envPrefix + "PASSWORD"); supplied {
							conn.Password = pass
						}
					}
					address := net.JoinHostPort(conn.Host, strconv.Itoa(port))
					dsn = fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable",
						url.UserPassword(conn.Username, conn.Password), address, url.PathEscape(conn.Database))
					create = "CREATE SCHEMA " + namespace
					cleanup = "DROP SCHEMA " + namespace + " CASCADE"
				}
				db, err := sql.Open(driver, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				if source.IsPGFamily(product) {
					var version string
					require.NoError(t, db.QueryRowContext(ctx, "SELECT version()").Scan(&version))
					actual := strings.ToLower(version)
					isProduct := strings.Contains(actual, product)
					if product == "highgo" {
						isProduct = isProduct || strings.Contains(actual, "hgdb") || strings.Contains(actual, "瀚高")
					}
					require.True(t, isProduct, "expected the actual %s server, got %s", product, version)
					t.Logf("%s fixture: %s", product, version)
				}
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
			src, sc, from := open(pair.source, "src")
			dst, dc, to := open(pair.target, "dst")
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
				"INSERT INTO "+account+" (id,title) VALUES (1,'preserve source'), (3,NULL)",
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
				"INSERT INTO "+targetAccount+" (id,"+titleColumn+") VALUES (2,'preserve target'), (4,NULL)",
			)
			type originalRow struct {
				id    int
				title sql.NullString
			}
			checkOriginalRows := func(db *sql.DB, table, column string, expected []originalRow) {
				t.Helper()
				rows, err := db.QueryContext(ctx, "SELECT id,"+column+" FROM "+table+" ORDER BY id")
				require.NoError(t, err)
				defer rows.Close()
				var actual []originalRow
				for rows.Next() {
					var row originalRow
					require.NoError(t, rows.Scan(&row.id, &row.title))
					actual = append(actual, row)
				}
				require.NoError(t, rows.Err())
				require.Equal(t, expected, actual)
			}
			originalSource := []originalRow{{1, sql.NullString{String: "preserve source", Valid: true}}, {id: 3}}
			originalTarget := []originalRow{{2, sql.NullString{String: "preserve target", Valid: true}}, {id: 4}}
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
			initialCount := len(initial.Changes)
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
			checkOriginalRows(src, account, "title", originalSource)
			checkOriginalRows(dst, targetAccount, titleColumn, originalTarget)
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
				require.NotContains(t, additive, change.ID, "selected difference should have disappeared: %+v", change)
				require.Equal(t, diff.Manual, change.Status, "%+v", change)
			}
			checkOriginalRows(src, account, "title", originalSource)
			checkOriginalRows(dst, targetAccount, titleColumn, originalTarget)
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(memo) FROM "+targetAccount).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+targetChild).Scan(&count))
			require.Zero(t, count)
			t.Logf("applied %d of %d initial differences, then %d remaining additive differences; applied differences disappeared and original rows were preserved",
				len(ids), initialCount, len(additive))
		})
	}
}
