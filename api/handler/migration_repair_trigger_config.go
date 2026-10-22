package handler

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
)

func validateRepairTriggerConfig(j *store.DataMigrationJob, cfg datamigrate.Config) error {
	for _, kind := range cfg.Objects {
		if !slices.Contains([]string{"primary_keys", "indexes", "sequences", "foreign_keys", "comments", "triggers"}, kind) {
			return fmt.Errorf("不支持的对象迁移类型 %s", kind)
		}
	}
	return validateTriggerMigration(j.SrcDBType, j.DstDBType, cfg.Content, cfg.MigrateTriggers || slices.Contains(cfg.Objects, "triggers"))
}

// A failed data load must be repaired before installing its business triggers.
// Target catalog validation alone cannot establish that prerequisite, nor can
// it detect a same-type column mapping that was exchanged after conversion.
func validateMySQLPGTriggerDependencies(jobID string, o repair.Object) error {
	if o.MySQLPGTrigger == nil {
		return nil
	}
	rows, err := store.ListMigrationRepairObjects(jobID)
	if err != nil {
		return err
	}
	objects := make([]repair.Object, 0, len(rows))
	for _, row := range rows {
		var item repair.Object
		if err = json.Unmarshal([]byte(row.ObjectJSON), &item); err != nil {
			return fmt.Errorf("读取触发器依赖记录: %w", err)
		}
		objects = append(objects, item)
	}
	return validateMySQLPGTriggerDependencyObjects(o, objects)
}

func validateMySQLPGTriggerDependencyObjects(o repair.Object, objects []repair.Object) error {
	if o.MySQLPGTrigger == nil {
		return nil
	}
	p := o.MySQLPGTrigger
	byID := map[string]repair.Object{}
	for _, item := range objects {
		byID[item.ID] = item
	}
	for _, id := range o.Dependencies {
		if _, ok := byID[id]; !ok {
			return fmt.Errorf("触发器依赖清单缺少对象 %s，请先修复依赖记录", id)
		}
	}
	for _, name := range p.Dependencies {
		var current *repair.Object
		for i := range objects {
			item := &objects[i]
			if item.Kind != "table" {
				continue
			}
			target := item.Table
			if target == "" {
				target = item.TargetName
			}
			if target != name {
				continue
			}
			if current != nil {
				return fmt.Errorf("触发器依赖表 %s 的修复记录不唯一", name)
			}
			current = item
		}
		// Dependencies outside this migration are verified from the live target
		// and the complete frozen table snapshot by CreateMySQLPGTrigger.
		if current == nil {
			continue
		}
		if current.Schema != p.Schema || current.TargetName != name {
			return fmt.Errorf("依赖表 %s 的目标身份已偏离触发器计划", name)
		}
		if current.DataStatus != "succeeded" && current.DataStatus != "not_required" {
			return fmt.Errorf("依赖表 %s 数据状态为 %s，请先完成数据补迁再安装触发器", name, current.DataStatus)
		}
		if current.Status != "succeeded" && !(len(current.Units) == 0 && current.DataStatus == "not_required" && current.Error == "" && (current.Status == "unknown" || current.Status == "pending" || current.Status == "reused")) {
			return fmt.Errorf("依赖表 %s 结构尚未成功，请先完成表修复", name)
		}
		var src, dst *source.TableDDLInfo
		for i := range p.SourceTables {
			table := &p.SourceTables[i]
			target := table.TableName
			if p.LowerCaseNames {
				target = strings.ToLower(target)
			}
			if target == name {
				src = table
			}
		}
		for i := range p.Tables {
			if p.Tables[i].TableName == name {
				dst = &p.Tables[i]
			}
		}
		if src == nil || dst == nil {
			return fmt.Errorf("触发器依赖表 %s 缺少冻结结构", name)
		}
		sourceName := current.SourceTable
		if sourceName == "" {
			sourceName = current.SourceName
		}
		if sourceName != src.TableName {
			return fmt.Errorf("依赖表 %s 的源表身份已改变", name)
		}
		if len(current.ColumnMapping) != len(src.Columns) {
			return fmt.Errorf("依赖表 %s 的列映射已偏离触发器计划，请重新迁移触发器", name)
		}
		for _, col := range src.Columns {
			expected := col.Name
			if p.LowerCaseNames {
				expected = strings.ToLower(expected)
			}
			if current.ColumnMapping[col.Name] != expected {
				return fmt.Errorf("依赖列 %s.%s 的映射已改变，请重新迁移触发器", name, col.Name)
			}
		}
		if len(current.Columns) != len(dst.Columns) {
			return fmt.Errorf("依赖表 %s 的冻结列集合已改变", name)
		}
		for _, expected := range dst.Columns {
			found := false
			for _, actual := range current.Columns {
				if actual.Name == expected.Name && strings.EqualFold(strings.Join(strings.Fields(actual.DataType), ""), strings.Join(strings.Fields(expected.DataType), "")) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("依赖列 %s.%s 已偏离触发器计划", name, expected.Name)
			}
		}
	}
	return nil
}

func validateMigrationRepairDependencies(jobID string, object repair.Object) error {
	if object.Native == nil {
		return validateMySQLPGTriggerDependencies(jobID, object)
	}
	rows, err := store.ListMigrationRepairObjects(jobID)
	if err != nil {
		return err
	}
	objects := map[string]repair.Object{}
	for _, row := range rows {
		var item repair.Object
		if err := json.Unmarshal([]byte(row.ObjectJSON), &item); err != nil {
			return err
		}
		objects[item.ID] = item
	}
	for _, id := range object.Dependencies {
		dependency, ok := objects[id]
		if !ok || dependency.Status != "succeeded" {
			return fmt.Errorf("原生对象依赖尚未成功: %s", id)
		}
		if dependency.DataStatus != "" && dependency.DataStatus != "succeeded" && dependency.DataStatus != "not_required" {
			return fmt.Errorf("原生对象依赖表的数据尚未迁移完成: %s", dependency.TargetName)
		}
	}
	return nil
}
