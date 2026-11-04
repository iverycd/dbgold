package handler

import (
	"fmt"
	"strings"
)

// resolveChangeOwner preserves explicit choices, including false. Only PostgreSQL
// compatible targets default to changing the owner to the schema's namesake role.
func resolveChangeOwner(targetType string, requested *bool) bool {
	if targetType == "oracle" {
		return false
	}
	if requested != nil {
		return *requested
	}
	switch targetType {
	case "postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase":
		return true
	default:
		return false
	}
}

// Native DM8 migration preserves the complete schema rather than table subsets.
func normalizeDamengNativeRequest(req *startDataMigrationRequest) error {
	if req.MigrateMode != "all" || strings.TrimSpace(req.TableFilter) != "" || len(req.ExcludedObjects) != 0 || (req.MigrateContent != "" && req.MigrateContent != "both") {
		return fmt.Errorf("达梦同库迁移只支持整个 Schema 的全部对象和数据，不能筛选表、排除对象或仅迁移结构/数据")
	}
	req.MigrateContent = "both"
	enabled := true
	req.MigrateTriggers = &enabled
	req.LowerCaseNames, req.CharInLength, req.UseNvarchar2, req.Distributed = false, false, false, false
	disabled := false
	req.ChangeOwner = &disabled
	req.StripViewSchemas = ""
	return nil
}

// Defaults apply only when creating a request. Saved jobs and repair Config
// retain their resolved bool so old migrations never gain triggers implicitly.
func resolveTriggerMigration(src, dst, content string, requested *bool) bool {
	if requested != nil {
		return *requested
	}
	return src == "mysql" && dst == "dameng" && content != "data_only"
}

// Streaming has only been verified on ordinary MySQL to GaussDB data loads.
func validateMySQLReadStrategy(strategy, src, dst, content string) error {
	if strategy != "stream" {
		return nil
	}
	if src != "mysql" || dst != "gaussdb" {
		return fmt.Errorf("流式读取仅支持 MySQL → GaussDB 迁移")
	}
	if content != "" && content != "both" && content != "data_only" {
		return fmt.Errorf("流式读取仅适用于包含表数据的迁移")
	}
	return nil
}
