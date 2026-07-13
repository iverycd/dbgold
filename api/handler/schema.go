package handler

import (
	"dbgold/driver"
	"dbgold/middleware"
	"dbgold/schema"
	"dbgold/store"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// routineExporter 是可选接口：支持导出自定义函数/存储过程原始 DDL 的 driver 实现它。
// 目前 mysql / oracle / sqlserver / dameng 实现，其余源库类型返回不支持。
type routineExporter interface {
	ExtractRoutines(dbName string) ([]schema.Routine, error)
}

type extractRequest struct {
	ConnectionID uint   `json:"connection_id" binding:"required"`
	Database     string `json:"database" binding:"required"`
}

func ExtractSchema(c *gin.Context) {
	var body extractRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, err := store.GetConnectionOwned(body.ConnectionID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	s, err := d.ExtractSchema(body.Database)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func ExtractFullSchema(c *gin.Context) {
	var body extractRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, err := store.GetConnectionOwned(body.ConnectionID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	s, err := d.ExtractFullObjects(body.Database)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

// ExportRoutines 将源库的自定义函数和存储过程原始 DDL 拼成单个 .sql 文件返回。
// 跨厂商函数/存储过程语法不兼容，此处只导出原样源码，不做任何转换，供用户手动适配目标库。
func ExportRoutines(c *gin.Context) {
	idStr := c.Query("connection_id")
	dbName := c.Query("database")
	if idStr == "" || dbName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connection_id and database required"})
		return
	}
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connection_id"})
		return
	}
	conn, err := store.GetConnectionOwned(uint(id), middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	re, ok := d.(routineExporter)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该源库类型暂不支持导出函数/存储过程: " + conn.DBType})
		return
	}
	if err := d.Connect(buildDSN(conn)); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	routines, err := re.ExtractRoutines(dbName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "-- 源库自定义函数/存储过程导出\n")
	fmt.Fprintf(&b, "-- 源库类型: %s    数据库/Schema: %s    对象数: %d\n", conn.DBType, dbName, len(routines))
	fmt.Fprintf(&b, "-- 注意: 以下为源库原始 DDL，未做跨库语法转换，迁移到目标库前请手动适配。\n\n")
	if len(routines) == 0 {
		b.WriteString("-- (该库中未发现自定义函数或存储过程)\n")
	}
	for _, r := range routines {
		b.WriteString("-- ============================================================\n")
		fmt.Fprintf(&b, "-- %s: %s\n", r.Type, r.Name)
		b.WriteString("-- ============================================================\n")
		b.WriteString(r.Body)
		b.WriteString("\n\n")
	}

	filename := fmt.Sprintf("%s_%s_routines.sql", conn.DBType, dbName)
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Data(http.StatusOK, "application/sql; charset=utf-8", []byte(b.String()))
}

func ParseDDLFile(c *gin.Context) {
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s, err := schema.ParseDDL(string(data))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func ExportDDL(c *gin.Context) {
	idStr := c.Query("connection_id")
	dbName := c.Query("database")
	if idStr == "" || dbName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connection_id and database required"})
		return
	}
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connection_id"})
		return
	}
	conn, err := store.GetConnectionOwned(uint(id), middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	s, err := d.ExtractFullObjects(dbName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}
