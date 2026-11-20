package datamigrate

import (
	"cmp"
	"regexp"
	"strings"

	"dbgold/datamigrate/dialect"
)

// The DM driver reports a scalar code as "Error -2665:" and an array
// diagnostic as "row[n]:-2665, ..." or "row[n]:Error -2665: ...".
// The same forms survive serialization in historical copy reports.
var damengLongRowError = regexp.MustCompile(`(?m)(?:^|:[ \t]+)Error -2665:|^[ \t]*row\[[0-9]+\]:[ \t]*(?:-2665,|Error -2665:)`)

// SuggestMySQLDamengLongRow adds manual advice to a MySQL -> DM data item.
// Only the live run knows whether it actually created this table.
func (item *ObjectResult) SuggestMySQLDamengLongRow(schema, table string, created bool) bool {
	if item.Suggestion != "" || item.SuggestedDDL != "" || (item.Stage != "" && item.Stage != "execute") || !damengLongRowError.MatchString(item.Error) {
		return false
	}
	if created {
		item.Suggestion = "本次建表时，含 VARCHAR2 或 NVARCHAR2 列的表已启用长行存储。请核对定长列和失败行各列的实际字节数，再判断是否需要调整列类型。"
		return true
	}
	schema = cmp.Or(item.TargetSchema, schema)
	table = cmp.Or(item.TargetTable, item.TargetName, table, item.Name, item.Table, item.SourceName)
	if strings.TrimSpace(schema) == "" || strings.TrimSpace(table) == "" {
		item.Suggestion = "请先核对达梦目标模式和表名，再检查该表的长行存储设置。若已启用，请核对定长列和失败行各列的实际字节数。"
		return true
	}
	item.SuggestedDDL = "ALTER TABLE " + dialect.NewDaMeng().QualifyTable(schema, table) + " ENABLE USING LONG ROW;"
	item.Suggestion = "请核对目标表的长行存储设置。若尚未启用，可手动执行建议 SQL，再从“去修复 → 接续迁移”补迁此表。若已启用，请核对定长列和失败行各列的实际字节数。"
	return true
}
