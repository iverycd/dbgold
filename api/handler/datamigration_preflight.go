package handler

import (
	"encoding/json"
	"net/http"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

func encodeExcludedObjects(objects []datamigrate.ObjectRef) string {
	if len(objects) == 0 {
		return ""
	}
	b, _ := json.Marshal(objects)
	return string(b)
}

type preflightDataMigrationRequest struct {
	startDataMigrationRequest
	MigrateObjects []string `json:"migrate_objects" binding:"omitempty,dive,oneof=primary_keys indexes sequences foreign_keys comments"`
	TableNames     []string `json:"table_names"`
	ViewNames      []string `json:"view_names"`
}

// PreflightDataMigration is observational. Run performs the same checks again;
// clients cannot supply a pre-approved DDL plan or bypass startup validation.
func PreflightDataMigration(c *gin.Context) {
	var req preflightDataMigrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, err := store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "源库连接不存在"})
		return
	}
	dst, err := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "目标库连接不存在"})
		return
	}
	native := isDamengNativePair(src.DBType, dst.DBType)
	if !native && (!source.IsPGFamily(src.DBType) || dst.DBType != "mysql") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "此预检仅支持 PG 系 → MySQL 或达梦 → 达梦"})
		return
	}
	if len(req.MigrateObjects) > 0 && len(req.ViewNames) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "对象和视图模式不能同时选择"})
		return
	}
	if native {
		if len(req.MigrateObjects) > 0 || len(req.TableNames) > 0 || len(req.ViewNames) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "达梦同库迁移仅支持整个 Schema 的全部对象和数据"})
			return
		}
		if err := normalizeDamengNativeRequest(&req.startDataMigrationRequest); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	reader, err := buildSrcReader(src, req.SrcDatabase, source.ConnPoolConfig{}, dst.DBType)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()
	writer, err := buildDstWriter(dst, req.TargetSchema, target.ConnPoolConfig{}, src.DBType)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer writer.Close()
	cfg := datamigrate.Config{MigrateTriggers: req.MigrateTriggers, OverwriteExisting: req.OverwriteExisting, Mode: req.MigrateMode, Filter: req.TableFilter, Content: req.MigrateContent, TargetSchema: req.TargetSchema, TargetDBType: dst.DBType, LowerCaseNames: req.LowerCaseNames, ChangeOwner: resolveChangeOwner(dst.DBType, req.ChangeOwner), Distributed: req.Distributed, PageSize: req.PageSize, ExcludedObjects: req.ExcludedObjects, Objects: req.MigrateObjects, TableNames: req.TableNames, ViewNames: req.ViewNames}
	m := datamigrate.NewMigrator(reader, writer, &datamigrate.Job{LogCh: make(chan string, 16)}, cfg)
	if native {
		c.JSON(http.StatusOK, m.PreflightDamengNative(c.Request.Context()))
		return
	}
	c.JSON(http.StatusOK, m.PreflightPGMySQL(c.Request.Context()))
}
