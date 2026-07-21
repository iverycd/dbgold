package handler

import (
	"context"
	"database/sql"
	"dbgold/middleware"
	"dbgold/store"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

var queryTimeout = 30 * time.Second
var queryMaxRows = 1000

func SetQueryConfig(timeoutSeconds, maxRows int) {
	if timeoutSeconds > 0 {
		queryTimeout = time.Duration(timeoutSeconds) * time.Second
	}
	if maxRows > 0 {
		queryMaxRows = maxRows
	}
}

type queryObject struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type queryColumn struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`
	Nullable   bool   `json:"nullable"`
	PrimaryKey bool   `json:"primary_key"`
}

type executeQueryRequest struct {
	ConnectionID     uint   `json:"connection_id" binding:"required"`
	Namespace        string `json:"namespace"`
	SQL              string `json:"sql" binding:"required"`
	Confirmed        bool   `json:"confirmed"`
	ConfirmationText string `json:"confirmation_text"`
}

type queryResultColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func ownedQueryConnection(c *gin.Context, id uint) (*store.Connection, bool) {
	conn, err := store.GetConnectionOwned(id, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return nil, false
	}
	if conn.DBType != "mysql" && conn.DBType != "postgres" && conn.DBType != "gaussdb" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("查询中心暂不支持 %s", conn.DBType)})
		return nil, false
	}
	return conn, true
}

func queryConnectionFromParam(c *gin.Context) (*store.Connection, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return nil, false
	}
	return ownedQueryConnection(c, uint(id))
}

func openQueryDB(conn *store.Connection) (*sql.DB, error) {
	driverName := "postgres"
	if conn.DBType == "mysql" {
		driverName = "mysql"
	}
	if conn.DBType == "gaussdb" {
		driverName = "opengauss"
	}
	return sql.Open(driverName, buildDSN(conn))
}

func ListQueryNamespaces(c *gin.Context) {
	conn, ok := queryConnectionFromParam(c)
	if !ok {
		return
	}
	db, err := openQueryDB(conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(c.Request.Context(), queryTimeout)
	defer cancel()
	var rows *sql.Rows
	if conn.DBType == "mysql" {
		rows, err = db.QueryContext(ctx, `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA
			WHERE SCHEMA_NAME NOT IN ('information_schema','mysql','performance_schema','sys') ORDER BY SCHEMA_NAME`)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT nspname FROM pg_namespace
			WHERE nspname <> 'information_schema' AND nspname NOT LIKE 'pg_%' ORDER BY nspname`)
	}
	if err != nil {
		writeQueryGatewayError(c, err)
		return
	}
	defer rows.Close()
	list := make([]string, 0)
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			list = append(list, name)
		}
	}
	if err = rows.Err(); err != nil {
		writeQueryGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

func ListQueryObjects(c *gin.Context) {
	conn, ok := queryConnectionFromParam(c)
	if !ok {
		return
	}
	namespace := strings.TrimSpace(c.Query("namespace"))
	if namespace == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace is required"})
		return
	}
	db, err := openQueryDB(conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(c.Request.Context(), queryTimeout)
	defer cancel()
	var rows *sql.Rows
	if conn.DBType == "mysql" {
		rows, err = db.QueryContext(ctx, `SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = ? ORDER BY TABLE_TYPE, TABLE_NAME`, namespace)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT table_name, table_type FROM information_schema.tables
			WHERE table_schema = $1 ORDER BY table_type, table_name`, namespace)
	}
	if err != nil {
		writeQueryGatewayError(c, err)
		return
	}
	defer rows.Close()
	objects := make([]queryObject, 0)
	for rows.Next() {
		var name, objectType string
		if rows.Scan(&name, &objectType) == nil {
			typ := "table"
			if strings.Contains(strings.ToUpper(objectType), "VIEW") {
				typ = "view"
			}
			objects = append(objects, queryObject{Name: name, Type: typ})
		}
	}
	if err = rows.Err(); err != nil {
		writeQueryGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, objects)
}

func ListQueryColumns(c *gin.Context) {
	conn, ok := queryConnectionFromParam(c)
	if !ok {
		return
	}
	namespace, objectName := strings.TrimSpace(c.Query("namespace")), strings.TrimSpace(c.Query("object"))
	if namespace == "" || objectName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace and object are required"})
		return
	}
	db, err := openQueryDB(conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(c.Request.Context(), queryTimeout)
	defer cancel()
	var rows *sql.Rows
	if conn.DBType == "mysql" {
		rows, err = db.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE = 'YES', COLUMN_KEY = 'PRI'
			FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, namespace, objectName)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT c.column_name, c.data_type, c.is_nullable = 'YES', EXISTS (
			SELECT 1 FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu ON tc.constraint_name = kcu.constraint_name
			 AND tc.constraint_schema = kcu.constraint_schema AND tc.table_name = kcu.table_name
			WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = c.table_schema
			 AND tc.table_name = c.table_name AND kcu.column_name = c.column_name)
			FROM information_schema.columns c WHERE c.table_schema = $1 AND c.table_name = $2 ORDER BY c.ordinal_position`, namespace, objectName)
	}
	if err != nil {
		writeQueryGatewayError(c, err)
		return
	}
	defer rows.Close()
	columns := make([]queryColumn, 0)
	for rows.Next() {
		var col queryColumn
		if rows.Scan(&col.Name, &col.DataType, &col.Nullable, &col.PrimaryKey) == nil {
			columns = append(columns, col)
		}
	}
	if err = rows.Err(); err != nil {
		writeQueryGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, columns)
}

func ExecuteQuery(c *gin.Context) {
	var req executeQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, ok := ownedQueryConnection(c, req.ConnectionID)
	if !ok {
		return
	}
	analysis, err := analyzeQuerySQL(req.SQL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	confirmationMode := ""
	if analysis.RiskLevel == "write" {
		confirmationMode = "click"
	}
	if analysis.RiskLevel == "dangerous" {
		confirmationMode = "click"
		if isProductionEnvironment(conn.Env) {
			confirmationMode = "type_connection_name"
		}
	}
	if confirmationMode != "" && (!req.Confirmed || (confirmationMode == "type_connection_name" && req.ConfirmationText != conn.Name)) {
		c.JSON(http.StatusConflict, gin.H{
			"error":             "该语句需要确认后执行",
			"code":              "confirmation_required",
			"risk_level":        analysis.RiskLevel,
			"confirmation_mode": confirmationMode,
			"statement_type":    analysis.StatementType,
			"connection_name":   conn.Name,
		})
		return
	}

	started := time.Now()
	audit := &store.QueryAudit{
		OwnerID: middleware.GetCurrentUserID(c), ConnectionID: conn.ID, ConnectionName: conn.Name,
		DBType: conn.DBType, Namespace: req.Namespace, SQLText: req.SQL,
		StatementType: analysis.StatementType, RiskLevel: analysis.RiskLevel,
		Confirmed: req.Confirmed, Status: "failed", ClientIP: c.ClientIP(), CreatedAt: started,
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), queryTimeout)
	defer cancel()
	db, err := openQueryDB(conn)
	if err != nil {
		finishQueryError(c, audit, started, err)
		return
	}
	defer db.Close()
	dbConn, err := db.Conn(ctx)
	if err != nil {
		finishQueryError(c, audit, started, err)
		return
	}
	defer dbConn.Close()
	if err = setQueryNamespace(ctx, dbConn, conn.DBType, req.Namespace); err != nil {
		finishQueryError(c, audit, started, err)
		return
	}

	if analysis.ReturnsRows {
		rows, queryErr := dbConn.QueryContext(ctx, req.SQL)
		if queryErr != nil {
			finishQueryError(c, audit, started, queryErr)
			return
		}
		defer rows.Close()
		columns, values, truncated, scanErr := scanQueryRows(rows, queryMaxRows)
		if scanErr != nil {
			finishQueryError(c, audit, started, scanErr)
			return
		}
		audit.Status, audit.RowCount, audit.Truncated = "success", int64(len(values)), truncated
		audit.DurationMS = time.Since(started).Milliseconds()
		persistQueryAudit(audit)
		c.JSON(http.StatusOK, gin.H{
			"kind": "rows", "columns": columns, "rows": values,
			"row_count": len(values), "truncated": truncated,
			"duration_ms": audit.DurationMS, "audit_id": audit.ID,
		})
		return
	}

	result, execErr := dbConn.ExecContext(ctx, req.SQL)
	if execErr != nil {
		finishQueryError(c, audit, started, execErr)
		return
	}
	affected, _ := result.RowsAffected()
	audit.Status, audit.AffectedRows = "success", affected
	audit.DurationMS = time.Since(started).Milliseconds()
	persistQueryAudit(audit)
	c.JSON(http.StatusOK, gin.H{
		"kind": "command", "affected_rows": affected,
		"duration_ms": audit.DurationMS, "audit_id": audit.ID,
	})
}

func setQueryNamespace(ctx context.Context, conn *sql.Conn, dbType, namespace string) error {
	if strings.TrimSpace(namespace) == "" {
		return nil
	}
	if dbType == "mysql" {
		_, err := conn.ExecContext(ctx, "USE `"+strings.ReplaceAll(namespace, "`", "``")+"`")
		return err
	}
	_, err := conn.ExecContext(ctx, `SELECT set_config('search_path', $1, false)`, namespace)
	return err
}

func scanQueryRows(rows *sql.Rows, maxRows int) ([]queryResultColumn, [][]any, bool, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, false, err
	}
	columns := make([]queryResultColumn, len(names))
	for i, name := range names {
		columns[i] = queryResultColumn{Name: name, Type: types[i].DatabaseTypeName()}
	}
	result := make([][]any, 0)
	truncated := false
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return nil, nil, false, err
		}
		if len(result) >= maxRows {
			truncated = true
			break
		}
		for i := range values {
			values[i] = normalizeQueryValue(values[i], types[i].DatabaseTypeName())
		}
		result = append(result, values)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return columns, result, truncated, nil
}

func normalizeQueryValue(value any, databaseType string) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case int64:
		if v > 9007199254740991 || v < -9007199254740991 {
			return strconv.FormatInt(v, 10)
		}
		return v
	case uint64:
		if v > 9007199254740991 {
			return strconv.FormatUint(v, 10)
		}
		return v
	case []byte:
		typ := strings.ToUpper(databaseType)
		binary := strings.Contains(typ, "BLOB") || strings.Contains(typ, "BINARY") || typ == "BYTEA" || !utf8.Valid(v)
		if binary {
			return "base64:" + base64.StdEncoding.EncodeToString(v)
		}
		return string(v)
	default:
		return value
	}
}

func finishQueryError(c *gin.Context, audit *store.QueryAudit, started time.Time, err error) {
	audit.DurationMS = time.Since(started).Milliseconds()
	audit.ErrorText = truncateQueryError(err.Error())
	persistQueryAudit(audit)
	status := http.StatusUnprocessableEntity
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		status = 499
	}
	c.JSON(status, gin.H{"error": err.Error(), "audit_id": audit.ID})
}

func persistQueryAudit(audit *store.QueryAudit) {
	if err := store.CreateQueryAudit(audit); err != nil {
		slog.Error("failed to persist query audit", "err", err)
	}
}

func truncateQueryError(message string) string {
	const max = 4000
	if len(message) <= max {
		return message
	}
	return message[:max]
}

func writeQueryGatewayError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func ListQueryHistory(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 || limit > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 100"})
		return
	}
	connectionID, err := strconv.ParseUint(c.DefaultQuery("connection_id", "0"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connection_id"})
		return
	}
	beforeID, err := strconv.ParseUint(c.DefaultQuery("before_id", "0"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid before_id"})
		return
	}
	allOwners := c.Query("scope") == "all" && middleware.IsAdmin(c)
	list, err := store.ListQueryAudits(store.QueryAuditFilter{
		OwnerID: middleware.GetCurrentUserID(c), AllOwners: allOwners,
		ConnectionID: uint(connectionID), Status: c.Query("status"),
		BeforeID: beforeID, Limit: limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}
