package handler

import (
	"database/sql"
	"dbgold/datamigrate/source"
	"dbgold/driver"
	"dbgold/middleware"
	"dbgold/store"
	"fmt"
	"net/http"
	"strconv"

	_ "gitee.com/chunanyong/dm"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

type connectionRequest struct {
	Name     string `json:"name" binding:"required"`
	DBType   string `json:"db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb dameng seabox highgo"`
	Host     string `json:"host" binding:"required"`
	Port     int    `json:"port" binding:"required,min=1,max=65535"`
	Database string `json:"database"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type updateConnectionRequest struct {
	Name     string `json:"name" binding:"required"`
	DBType   string `json:"db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb dameng seabox highgo"`
	Host     string `json:"host" binding:"required"`
	Port     int    `json:"port" binding:"required,min=1,max=65535"`
	Database string `json:"database"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password"`
}

// connectionWithOwner 在连接基础上附带所属用户名，仅 admin 视角返回。
type connectionWithOwner struct {
	store.Connection
	OwnerUsername string `json:"owner_username"`
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
		// 使用 key=value 格式，避免密码含 @ 或 host 含 \ 破坏 URL 解析
		return fmt.Sprintf("server=%s;port=%d;database=%s;user id=%s;password=%s;trustservercertificate=true;encrypt=DISABLE",
			c.Host, c.Port, c.Database, c.Username, c.Password)
	case "gaussdb":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			c.Host, c.Port, c.Username, c.Password, c.Database)
	case "seabox":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			c.Host, c.Port, c.Username, c.Password, c.Database)
	case "highgo":
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			c.Host, c.Port, c.Username, c.Password, c.Database)
	case "dameng":
		return fmt.Sprintf("dm://%s:%s@%s:%d",
			c.Username, c.Password, c.Host, c.Port)
	}
	return ""
}

// getOwnedConnection 取连接并做归属校验：普通用户只能访问自己的连接，admin 可访问任意。
// 非归属或不存在统一返回 404（不暴露存在性）。校验失败时已写入响应，调用方应直接 return。
func getOwnedConnection(c *gin.Context, id uint) (*store.Connection, bool) {
	conn, err := store.GetConnection(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return nil, false
	}
	if !middleware.IsAdmin(c) && conn.OwnerID != middleware.GetCurrentUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return nil, false
	}
	return conn, true
}

func GetConnections(c *gin.Context) {
	isAdmin := middleware.IsAdmin(c)
	list, err := store.ListConnections(middleware.GetCurrentUserID(c), isAdmin)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// admin 视角附带「所属用户」用户名，便于前端展示归属
	if isAdmin {
		users, err := store.ListUsers()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		nameByID := make(map[uint]string, len(users))
		for _, u := range users {
			nameByID[u.ID] = u.Username
		}
		resp := make([]connectionWithOwner, len(list))
		for i := range list {
			resp[i] = connectionWithOwner{Connection: list[i], OwnerUsername: nameByID[list[i].OwnerID]}
		}
		c.JSON(http.StatusOK, resp)
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
		OwnerID: middleware.GetCurrentUserID(c),
		Name:    body.Name, DBType: body.DBType,
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
	if _, ok := getOwnedConnection(c, uint(id)); !ok {
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
	if _, ok := getOwnedConnection(c, uint(id)); !ok {
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
	conn, ok := getOwnedConnection(c, uint(id))
	if !ok {
		return
	}
	d, err := driver.NewDriver(conn.DBType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer d.Close()
	if err := d.Connect(buildDSN(conn)); err != nil {
		c.Error(err)
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
	conn, ok := getOwnedConnection(c, uint(id))
	if !ok {
		return
	}
	var reader source.Reader
	switch conn.DBType {
	case "mysql":
		reader, err = source.NewMySQL(buildDSN(conn), conn.Database, source.ConnPoolConfig{})
	case "sqlserver":
		reader, err = source.NewSQLServer(buildDSN(conn), conn.Database, source.ConnPoolConfig{})
	case "dameng":
		reader, err = source.NewDaMeng(buildDSN(conn), conn.Database, source.ConnPoolConfig{})
	case "oracle":
		reader, err = source.NewOracle(buildDSN(conn), conn.Database, source.ConnPoolConfig{})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("不支持列出 %s 类型的数据库", conn.DBType)})
		return
	}
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()
	dbs, err := reader.ListDatabases(c.Request.Context())
	if err != nil {
		c.Error(err)
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
	conn, ok := getOwnedConnection(c, uint(id))
	if !ok {
		return
	}

	// 达梦的 schema 即数据库用户,单独走 ALL_USERS 查询
	if conn.DBType == "dameng" {
		listDaMengSchemas(c, conn)
		return
	}

	if conn.DBType != "postgres" && conn.DBType != "gaussdb" && conn.DBType != "seabox" && conn.DBType != "highgo" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("不支持列出 %s 类型的 schema", conn.DBType)})
		return
	}

	driverName := "postgres"
	if conn.DBType == "gaussdb" {
		driverName = "opengauss"
	}
	// seabox 使用标准 postgres 驱动
	db, err := sql.Open(driverName, buildDSN(conn))
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(c.Request.Context(),
		`SELECT nspname FROM pg_namespace WHERE nspname NOT IN (
		   'information_schema','pg_catalog','pg_toast',
		   'cstore','dbe_perf','snapshot','blockchain','db4ai','prvt_ilm','sys',
		   'dbe_ilm_admin','sqladvisor','dbe_pldebugger','dbe_pldeveloper','dbe_sql_util',
		   'pkg_util','dbe_scheduler','pkg_service','dbe_raw','dbe_utility','dbe_output',
		   'dbe_xml','dbe_xmldom','dbe_xmlparser','dbe_describe','dbe_stats','dbe_profiler',
		   'dbe_heat_map','dbe_ilm','dbe_compression','dbe_xmlgen','resource_manager',
		   'dbe_file','dbe_random','dbe_application_info','dbe_sql','dbe_lob','dbe_task',
		   'dbe_match','dbe_session',
		   'pg_aoseg','pg_bitmapindex','sc_toolkit','stat_perf',
		   '_seaboxts_catalog','_seaboxts_internal','seaboxts_information','sdaudit',
		   'pg_temp_1','pg_toast_temp_1',
		   'dbms_pipe','dbms_alert','plvdate','plvstr','plvchr','dbms_output',
		   'plvsubst','dbms_utility','plvlex','utl_file','dbms_assert','dbms_random',
		   'oracle','plunit','wmsys','mysql'
		 )`)
	if err != nil {
		c.Error(err)
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

// listDaMengSchemas 列出达梦可作为目标的 schema(即数据库用户),
// 过滤掉达梦内置系统用户。
func listDaMengSchemas(c *gin.Context, conn *store.Connection) {
	db, err := sql.Open("dm", buildDSN(conn))
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(c.Request.Context(),
		`SELECT USERNAME FROM ALL_USERS
		 WHERE USERNAME NOT IN (
		   'SYS','SYSDBA','SYSAUDITOR','SYSSSO','SYSDBO',
		   'CTISYS','SYSUSERS'
		 )
		 ORDER BY USERNAME`)
	if err != nil {
		c.Error(err)
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
