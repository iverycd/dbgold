package datamigrate

import (
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/repair"
)

// Only the preparation coordinator calls this callback. Always report the last
// result, even when the entire read finishes inside the one-second interval.
func newRepairMetadataProgress(log *Logger, now func() time.Time) func(int, int) {
	started := now()
	last := started
	return func(done, total int) {
		current := now()
		if done != total && current.Sub(last) < time.Second {
			return
		}
		last = current
		log.Infof("读取表结构：%d/%d，耗时 %s", done, total, current.Sub(started).Round(time.Millisecond))
	}
}

func startRepairPreparationStage(log *Logger, name string) func() {
	started := time.Now()
	log.Infof("正在%s…", name)
	return func() {
		log.Infof("%s完成，耗时 %s", name, time.Since(started).Round(time.Millisecond))
	}
}

type repairReferenceKey struct {
	table   string
	columns string
}

func repairReference(table string, columns []string) repairReferenceKey {
	var signature strings.Builder
	for _, column := range columns {
		signature.WriteString(strconv.Itoa(len(column)))
		signature.WriteByte(':')
		signature.WriteString(column)
	}
	return repairReferenceKey{table: table, columns: signature.String()}
}

// Build before exclusions: the original ordered scan chose the first matching
// key, including a key that is subsequently excluded from execution.
func linkRepairForeignKeys(objects []repair.Object) {
	keys := make(map[repairReferenceKey]string)
	for _, object := range objects {
		if object.Index == nil || (!object.Index.IsPrimary && !object.Index.IsUnique) {
			continue
		}
		key := repairReference(object.Table, object.Index.Columns)
		if _, exists := keys[key]; !exists {
			keys[key] = object.ID
		}
	}
	for i := range objects {
		object := &objects[i]
		if object.ForeignKey == nil {
			continue
		}
		if id, exists := keys[repairReference(object.ForeignKey.RefTable, object.ForeignKey.RefColumns)]; exists {
			object.Dependencies = append(object.Dependencies, id)
		}
	}
}

// Build after exclusions. A later table with the same target name wins, as in
// the original scan; each recipient still owns its own non-nil mapping.
func attachRepairColumnMappings(objects []repair.Object) {
	tables := make(map[string]map[string]string)
	for _, object := range objects {
		if object.Kind == "table" {
			tables[object.Table] = object.ColumnMapping
		}
	}
	for i := range objects {
		object := &objects[i]
		if object.Kind == "table" {
			continue
		}
		if object.ForeignKey != nil {
			if mapping, exists := tables[object.ForeignKey.RefTable]; exists {
				object.RefColumnMapping = copyRepairColumnMapping(mapping)
			}
		}
		if mapping, exists := tables[object.Table]; exists {
			object.ColumnMapping = copyRepairColumnMapping(mapping)
		}
	}
}

func copyRepairColumnMapping(mapping map[string]string) map[string]string {
	copy := make(map[string]string, len(mapping))
	for source, target := range mapping {
		copy[source] = target
	}
	return copy
}
