package datamigrate

import "dbgold/datamigrate/repair"

func repairDiagnostic(item ObjectResult) bool {
	if item.Stage == "metadata" || item.Stage == "diagnostic" {
		return true
	}
	// Older reports did not mark global failures with a stage. Only recognize
	// their reserved labels when there is no executable object definition.
	if item.DDL != "" || item.Table != "" || item.SuggestedDDL != "" {
		return false
	}
	switch item.Name {
	case "元数据读取", "源表列表", "修复记录持久化", "视图范围", "视图模式", "视图依赖表", "视图元数据", "视图名称预检", "迁移准备", "兼容预检", "Oracle 迁移预检", "Oracle 预检", "Oscar 对象命名预检", "目标 schema", "主键元数据", "自增元数据", "索引元数据", "外键元数据", "注释元数据", "读取 MySQL 视图数值精度":
		return true
	}
	return false
}

// EnrichRepairReferences also supports ledgers created before reference fields
// existed. It never replaces drafts, execution states, or target identities.
func EnrichRepairReferences(objects []repair.Object, report MigrationReport) {
	byName := map[[2]string][]repair.Object{}
	for _, ref := range LegacyRepairObjects(report, "") {
		key := [2]string{ref.Kind, ref.Name}
		byName[key] = append(byName[key], ref)
	}
	for i := range objects {
		o := &objects[i]
		var matches []repair.Object
		seen := map[string]bool{}
		for _, name := range []string{o.Name, o.SourceName, o.TargetName} {
			for _, ref := range byName[[2]string{o.Kind, name}] {
				if seen[ref.ID] || !ref.DiagnosticOnly && ref.Table != "" && ref.Table != o.Table && ref.Table != o.SourceTable {
					continue
				}
				seen[ref.ID] = true
				matches = append(matches, ref)
			}
		}
		if len(matches) == 1 {
			if o.SourceDDL == "" {
				o.SourceDDL = matches[0].SourceDDL
			}
			o.DiagnosticOnly = o.DiagnosticOnly || matches[0].DiagnosticOnly
		}
	}
}
