package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dbgold/datamigrate/source"
	"dbgold/driver"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

type updateConnectionDatabaseRequest struct {
	Database         string  `json:"database" binding:"required"`
	ExpectedDatabase *string `json:"expected_database" binding:"required"`
}

func supportsConnectionDatabase(dbType string) bool {
	return source.IsPGFamily(dbType) || dbType == "mysql" || dbType == "sqlserver" || dbType == "oscar"
}

func testDatabaseConnection(ctx context.Context, conn *store.Connection) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d, err := driver.NewDriver(conn.DBType)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Connect(ctx, driver.ConnectOptions{
		DSN: buildDSN(conn), Username: conn.Username, Password: conn.Password,
	})
}

// UpdateConnectionDatabase tests the candidate first and persists only its
// database. Neither failed tests nor stale editors can change the connection.
func UpdateConnectionDatabase(c *gin.Context) {
	updateConnectionDatabase(c, testDatabaseConnection)
}

func updateConnectionDatabase(c *gin.Context, test func(context.Context, *store.Connection) error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, ok := getOwnedConnection(c, uint(id))
	if !ok {
		return
	}
	if !supportsConnectionDatabase(conn.DBType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该数据库类型不支持修改连接数据库，请使用 Schema/用户选择"})
		return
	}
	var req updateConnectionDatabaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Database = strings.TrimSpace(req.Database)
	if req.Database == "" || strings.IndexFunc(req.Database, unicode.IsControl) >= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据库名称不能为空或包含控制字符"})
		return
	}
	if conn.DBType == "oscar" && strings.ContainsAny(req.Database, "/?;") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Oscar 数据库名称不能包含 JDBC 连接分隔符 /、? 或 ;"})
		return
	}
	if conn.Database != *req.ExpectedDatabase {
		c.JSON(http.StatusConflict, gin.H{"error": store.ErrConnectionChanged.Error()})
		return
	}
	if req.Database == conn.Database {
		c.JSON(http.StatusOK, conn)
		return
	}
	if err := store.CheckConnectionDatabaseUse(conn.ID); err != nil {
		connectionDatabaseError(c, err)
		return
	}
	candidate := *conn
	candidate.Database = req.Database
	if err := test(c.Request.Context(), &candidate); err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("连接数据库失败，未保存修改: %v", err)})
		return
	}

	// Network testing must not hold this lock. The store rechecks the exact
	// tested endpoint/login and active CDC references in the save transaction.
	incrementalStartMu.Lock()
	updated, err := store.UpdateConnectionDatabase(conn, req.Database)
	incrementalStartMu.Unlock()
	if err != nil {
		connectionDatabaseError(c, err)
		return
	}
	c.JSON(http.StatusOK, updated)
}

func connectionDatabaseError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, store.ErrConnectionChanged) || errors.Is(err, store.ErrConnectionInUse) {
		status = http.StatusConflict
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

// ListConnectionCatalogs lists real databases. The older /databases endpoint
// intentionally retains its source migration-unit (database or schema) meaning.
func ListConnectionCatalogs(c *gin.Context) {
	listConnectionCatalogs(c, queryConnectionCatalogs)
}

func listConnectionCatalogs(c *gin.Context, query func(context.Context, *store.Connection) ([]string, error)) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, ok := getOwnedConnection(c, uint(id))
	if !ok {
		return
	}
	if !supportsConnectionDatabase(conn.DBType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该数据库类型不支持列出连接数据库"})
		return
	}
	if conn.DBType == "oscar" {
		c.JSON(http.StatusOK, []string{})
		return
	}
	catalogs, err := query(c.Request.Context(), conn)
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("读取数据库列表失败，可手动输入数据库名称: %v", err)})
		return
	}
	if catalogs == nil {
		catalogs = []string{}
	}
	c.JSON(http.StatusOK, catalogs)
}

func connectionCatalogQuery(dbType string) (string, string) {
	switch dbType {
	case "mysql":
		return "mysql", `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA
			WHERE SCHEMA_NAME NOT IN ('information_schema','mysql','performance_schema','sys')
			ORDER BY SCHEMA_NAME`
	case "sqlserver":
		return "sqlserver", `SELECT name FROM sys.databases
			WHERE name NOT IN ('master','tempdb','model','msdb')
			  AND state = 0 AND HAS_DBACCESS(name) = 1
			ORDER BY name`
	default:
		return pgCompatibleDriverName(dbType), `SELECT datname FROM pg_database
			WHERE NOT datistemplate AND datallowconn
			  AND has_database_privilege(current_user, datname, 'CONNECT')
			ORDER BY datname`
	}
}

func queryConnectionCatalogs(ctx context.Context, conn *store.Connection) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	driverName, query := connectionCatalogQuery(conn.DBType)
	db, err := sql.Open(driverName, buildDSN(conn))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return readConnectionCatalogs(ctx, db, query)
}

func readConnectionCatalogs(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	catalogs := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		catalogs = append(catalogs, name)
	}
	return catalogs, rows.Err()
}
