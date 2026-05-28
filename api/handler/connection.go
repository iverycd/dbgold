package handler

import (
	"database/sql"
	"dbgold/datamigrate/source"
	"dbgold/driver"
	"dbgold/store"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

type connectionRequest struct {
	Name     string `json:"name" binding:"required"`
	DBType   string `json:"db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb"`
	Host     string `json:"host" binding:"required"`
	Port     int    `json:"port" binding:"required,min=1,max=65535"`
	Database string `json:"database" binding:"required"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type updateConnectionRequest struct {
	Name     string `json:"name" binding:"required"`
	DBType   string `json:"db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb"`
	Host     string `json:"host" binding:"required"`
	Port     int    `json:"port" binding:"required,min=1,max=65535"`
	Database string `json:"database" binding:"required"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password"`
}

func buildDSN(c *store.Connection) string {
	switch c.DBType {
	case "mysql":
		return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4",
			c.Username, c.Password, c.Host, c.Port, c.Database)
	case "postgres":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			c.Host, c.Port, c.Username, c.Password, c.Database)
	case "oracle":
		return fmt.Sprintf("oracle://%s:%s@%s:%d/%s",
			c.Username, c.Password, c.Host, c.Port, c.Database)
	case "sqlserver":
		return fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s",
			c.Username, c.Password, c.Host, c.Port, c.Database)
	case "gaussdb":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			c.Host, c.Port, c.Username, c.Password, c.Database)
	}
	return ""
}

func GetConnections(c *gin.Context) {
	list, err := store.ListConnections()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

func CreateConnection(c *gin.Context) {
	var body connectionRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn := &store.Connection{
		Name: body.Name, DBType: body.DBType,
		Host: body.Host, Port: body.Port,
		Database: body.Database, Username: body.Username, Password: body.Password,
	}
	if err := store.CreateConnection(conn); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, conn)
}

func UpdateConnection(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var body updateConnectionRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{
		"name": body.Name, "db_type": body.DBType,
		"host": body.Host, "port": body.Port,
		"database": body.Database, "username": body.Username,
	}
	if body.Password != "" {
		updates["password"] = body.Password
	}
	if err := store.UpdateConnection(uint(id), updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "updated"})
}

func DeleteConnection(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := store.DeleteConnection(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func TestConnection(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, err := store.GetConnection(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	d, err := driver.NewDriver(conn.DBType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer d.Close()
	if err := d.Connect(buildDSN(conn)); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "connection successful"})
}

func ListConnectionDatabases(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, err := store.GetConnection(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	var reader source.Reader
	switch conn.DBType {
	case "mysql":
		reader, err = source.NewMySQL(buildDSN(conn), conn.Database)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("不支持列出 %s 类型的数据库", conn.DBType)})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()
	dbs, err := reader.ListDatabases(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, dbs)
}

func ListConnectionSchemas(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	conn, err := store.GetConnection(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	if conn.DBType != "postgres" && conn.DBType != "gaussdb" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("不支持列出 %s 类型的 schema", conn.DBType)})
		return
	}

	driverName := "postgres"
	if conn.DBType == "gaussdb" {
		driverName = "opengauss"
	}
	db, err := sql.Open(driverName, buildDSN(conn))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(c.Request.Context(),
		`SELECT schema_name FROM information_schema.schemata
		 WHERE schema_name NOT IN (
		   'information_schema','pg_catalog','pg_toast',
		   'cstore','dbe_perf','snapshot','blockchain','db4ai','prvt_ilm','sys',
		   'dbe_ilm_admin','sqladvisor','dbe_pldebugger','dbe_pldeveloper','dbe_sql_util',
		   'pkg_util','dbe_scheduler','pkg_service','dbe_raw','dbe_utility','dbe_output',
		   'dbe_xml','dbe_xmldom','dbe_xmlparser','dbe_describe','dbe_stats','dbe_profiler',
		   'dbe_heat_map','dbe_ilm','dbe_compression','dbe_xmlgen','resource_manager',
		   'dbe_file','dbe_random','dbe_application_info','dbe_sql','dbe_lob','dbe_task',
		   'dbe_match','dbe_session'
		 )
		 ORDER BY schema_name`)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			continue
		}
		schemas = append(schemas, s)
	}
	c.JSON(http.StatusOK, schemas)
}
