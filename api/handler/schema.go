package handler

import (
	"dbgold/driver"
	"dbgold/middleware"
	"dbgold/schema"
	"dbgold/store"
	"fmt"
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

// triggerExporter 是可选接口：支持导出触发器原始 DDL 的 driver 实现它。
// 与 routineExporter 分离，避免扩展通用 Driver 接口影响不支持该能力的源库。
type triggerExporter interface {
	ExtractTriggers(dbName string) ([]schema.Routine, error)
}

// ExportRoutines 将源库的自定义函数、存储过程和触发器原始 DDL 拼成单个 .sql 文件返回。
// 跨厂商语法不兼容，此处只导出原样源码，不做任何转换，供用户手动适配目标库。
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "该源库类型暂不支持导出函数/存储过程/触发器: " + conn.DBType})
		return
	}
	te, ok := d.(triggerExporter)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该源库类型暂不支持导出函数/存储过程/触发器: " + conn.DBType})
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
	triggers, err := te.ExtractTriggers(dbName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	content := buildRoutineTriggerSQL(conn.DBType, dbName, routines, triggers)
	filename := fmt.Sprintf("%s_%s_routines_triggers.sql", conn.DBType, dbName)
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Data(http.StatusOK, "application/sql; charset=utf-8", []byte(content))
}

func buildRoutineTriggerSQL(dbType, dbName string, routines, triggers []schema.Routine) string {
	var b strings.Builder
	b.WriteString("-- 源库自定义函数/存储过程/触发器导出\n")
	fmt.Fprintf(&b, "-- 源库类型: %s    数据库/Schema: %s\n", dbType, dbName)
	fmt.Fprintf(&b, "-- 函数/存储过程/包数量: %d    触发器数量: %d\n", len(routines), len(triggers))
	b.WriteString("-- 注意: 以下为源库原始 DDL，未做跨库语法转换，迁移到目标库前请手动适配。\n\n")

	b.WriteString("-- 函数 / 存储过程 / 包\n\n")
	if len(routines) == 0 {
		b.WriteString("-- (该库中未发现自定义函数、存储过程或包)\n\n")
	} else {
		writeExportObjects(&b, routines)
	}

	b.WriteString("-- 触发器\n\n")
	if len(triggers) == 0 {
		b.WriteString("-- (该库中未发现触发器)\n")
	} else {
		writeExportObjects(&b, triggers)
	}
	return b.String()
}

func writeExportObjects(b *strings.Builder, objects []schema.Routine) {
	for _, object := range objects {
		b.WriteString("-- ============================================================\n")
		fmt.Fprintf(b, "-- %s: %s\n", object.Type, object.Name)
		b.WriteString("-- ============================================================\n")
		b.WriteString(object.Body)
		b.WriteString("\n\n")
	}
}
