package handler

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/store"
)

// Eligibility is derived from the owned job and persisted identity. A response
// capability is never accepted as execution authority from a draft request.
func canEditManualTrigger(j *store.DataMigrationJob, o repair.Object) bool {
	if j.DstDBType != "dameng" || o.Kind != "trigger" || o.Schema == "" || o.TargetName == "" {
		return false
	}
	if !slices.Contains([]string{"failed", "pending", "draft", "blocked", "cancelled", "rolled_back"}, o.Status) {
		return false
	}
	if slicesContainUncertainManualUnits(o.Units) {
		return false
	}
	if o.Native != nil && (o.Native.Managed || o.Native.Generated || o.Native.EventTrigger || o.Native.Kind != "trigger") {
		return false
	}
	if o.Native != nil && o.Native.Table != manualTriggerTable(o) {
		return false
	}
	if manualTriggerTable(o) == "" {
		return false
	}
	for _, sql := range manualTriggerOriginalSQL(o) {
		if kind, _, err := damengnative.EventTriggerScope(sql, o.Schema); err == nil && kind != "table" {
			return false
		}
		// The original header is useful even when its body failed compilation.
		if strings.Contains(strings.ToUpper(trimRepairComments(sql)), "INSTEAD OF") {
			if def, err := damengnative.ParseTableTriggerHeader(sql, o.Schema); err != nil || def.Table == "" {
				return false
			}
		}
	}
	return true
}

func manualTriggerOriginalSQL(o repair.Object) []string {
	out := []string{}
	if o.ManualTrigger != nil && o.ManualTrigger.SQL != "" {
		out = append(out, o.ManualTrigger.SQL)
	}
	for _, unit := range o.Units {
		kind, _, _, err := repairHead(unit.SQL)
		if err == nil && kind == "trigger" {
			out = append(out, unit.SQL)
		}
	}
	if o.DDL != "" {
		out = append(out, o.DDL)
	}
	return out
}

func manualTriggerTable(o repair.Object) string {
	if o.Table != "" {
		return o.Table
	}
	if o.Native != nil && o.Native.Table != "" {
		return o.Native.Table
	}
	for _, sql := range manualTriggerOriginalSQL(o) {
		if def, err := damengnative.ParseTableTriggerHeader(sql, o.Schema); err == nil && def.Schema == o.Schema && def.Name == o.TargetName && def.TableSchema == o.Schema {
			return def.Table
		}
	}
	return ""
}

func repairObjectResponse(j *store.DataMigrationJob, o repair.Object) repair.Object {
	o.ManualTriggerEditable = canEditManualTrigger(j, o)
	if o.ManualTriggerEditable && o.Table == "" {
		o.Table = manualTriggerTable(o)
	}
	return o
}

func slicesContainUncertainManualUnits(units []repair.Unit) bool {
	for _, unit := range units {
		if slices.Contains([]string{"unknown", "running", "queued", "unverified", "needs_verification"}, unit.Status) {
			return true
		}
	}
	return false
}

func prepareManualTriggerDraft(j *store.DataMigrationJob, o repair.Object, units []repair.Unit, reviewed bool) (repair.Object, error) {
	if !canEditManualTrigger(j, o) {
		return o, fmt.Errorf("仅结果确定且尚未成功的达梦表触发器支持手工编辑")
	}
	if len(units) != 1 || strings.TrimSpace(units[0].SQL) == "" || len(units[0].SQL) > 1<<20 {
		return o, fmt.Errorf("表触发器须填写一个完整且不超过 1 MiB 的 SQL 执行单元")
	}
	def, err := damengnative.ParseTableTriggerSQL(units[0].SQL, o.Schema)
	if err != nil {
		return o, err
	}
	table := manualTriggerTable(o)
	if def.Schema != o.Schema || def.Name != o.TargetName || def.TableSchema != o.Schema || def.Table != table {
		return o, fmt.Errorf("触发器必须保持原目标 %s.%s 及所属表 %s.%s", o.Schema, o.TargetName, o.Schema, table)
	}
	// Deep-copy before revising: the first snapshot and all frozen source plans
	// remain immutable across successive saves and asynchronous attempts.
	var current repair.Object
	if err = json.Unmarshal([]byte(repairJSON(o)), &current); err != nil {
		return o, err
	}
	if current.ManualTrigger == nil {
		var original repair.Object
		if err = json.Unmarshal([]byte(repairJSON(o)), &original); err != nil {
			return o, err
		}
		original.ManualTriggerEditable = false
		current.ManualTrigger = &repair.ManualTriggerPlan{Original: &original}
	}
	plan := current.ManualTrigger
	changed := plan.SQL != def.SQL
	if changed {
		if plan.SQL != "" && !slices.Contains(plan.PreviousSQL, plan.SQL) {
			plan.PreviousSQL = append(plan.PreviousSQL, plan.SQL)
		}
		plan.Revision++
		plan.SQL = def.SQL
		current.Units = nil
		if current.Native != nil {
			if !o.TrustedNative() {
				return o, fmt.Errorf("原生触发器计划身份或指纹不一致")
			}
			creates := 0
			for i := range current.Native.Units {
				u := &current.Native.Units[i]
				kind, _, _, headErr := repairHead(u.SQL)
				if headErr == nil && kind == "trigger" {
					u.SQL = def.SQL
					creates++
				}
				current.Units = append(current.Units, repair.Unit{ID: fmt.Sprintf("manual-%d-%d", plan.Revision, i+1), SQL: u.SQL, OriginalSQL: u.SQL, Status: "pending", Selected: true})
			}
			if creates != 1 {
				return o, fmt.Errorf("原生表触发器缺少唯一 CREATE 执行单元")
			}
			current.NativeFingerprint = damengnative.Fingerprint(current.Native)
		} else {
			current.Units = []repair.Unit{{ID: fmt.Sprintf("manual-%d", plan.Revision), SQL: def.SQL, OriginalSQL: def.SQL, Status: "pending", Selected: true}}
		}
	}
	// Generated conversion metadata is archived in Original, and cannot route a
	// user-authored program back through the immutable converter executor.
	current.MySQLDamengTrigger = nil
	current.MySQLPGTrigger = nil
	current.Table = table
	current.DiagnosticOnly = false
	current.ManualTriggerEditable = false
	current.RequiresReview = !reviewed
	current.Status = "draft"
	current.Verification, current.ManualVerification = nil, nil
	for i := range current.Units {
		current.Units[i].Selected = current.Units[i].Status != "succeeded"
	}
	if !current.TrustedManualTrigger() {
		return o, fmt.Errorf("手工触发器修订与完整执行计划不一致")
	}
	return current, nil
}

func validateManualTriggerDependencies(jobID string, object repair.Object) error {
	rows, err := store.ListMigrationRepairObjects(jobID)
	if err != nil {
		return err
	}
	objects := map[string]repair.Object{}
	for _, row := range rows {
		var item repair.Object
		if err = json.Unmarshal([]byte(row.ObjectJSON), &item); err != nil {
			return err
		}
		objects[item.ID] = item
	}
	for _, id := range object.Dependencies {
		dependency, found := objects[id]
		installed := found && (dependency.Status == "succeeded" || dependency.Native != nil && nativeRecoveryInstallationComplete(dependency))
		// Object-only jobs freeze table metadata without executing CREATE TABLE.
		// Its existence and type are verified from the target by the executor.
		metadataOnly := found && dependency.Kind == "table" && len(dependency.Units) == 0 && dependency.DataStatus == "not_required" && dependency.Error == "" && slices.Contains([]string{"unknown", "pending", "reused"}, dependency.Status)
		if !installed && !metadataOnly {
			return fmt.Errorf("触发器依赖尚未成功: %s", id)
		}
		if dependency.DataStatus != "" && dependency.DataStatus != "succeeded" && dependency.DataStatus != "not_required" {
			return fmt.Errorf("触发器依赖 %s 数据尚未完成", dependency.TargetName)
		}
	}
	for _, dependency := range objects {
		if dependency.Schema != object.Schema || dependency.Kind != "table" && dependency.Kind != "data" || dependency.Table != object.Table && dependency.TargetName != object.Table {
			continue
		}
		if dependency.Kind == "table" && dependency.Status != "succeeded" && !(len(dependency.Units) == 0 && dependency.DataStatus == "not_required" && dependency.Error == "") {
			return fmt.Errorf("触发器所属表 %s 结构尚未成功", object.Table)
		}
		status := dependency.DataStatus
		if dependency.Kind == "data" && status == "" {
			status = dependency.Status
		}
		if status != "" && status != "succeeded" && status != "not_required" {
			return fmt.Errorf("触发器所属表 %s 数据尚未完成", object.Table)
		}
	}
	return nil
}

func bindManualTriggerColumnMapping(jobID string, object *repair.Object) error {
	rows, err := store.ListMigrationRepairObjects(jobID)
	if err != nil {
		return err
	}
	found := false
	for _, row := range rows {
		var table repair.Object
		if err = json.Unmarshal([]byte(row.ObjectJSON), &table); err != nil {
			return err
		}
		if table.Kind != "table" || table.Schema != object.Schema || table.TargetName != object.Table {
			continue
		}
		if found {
			return fmt.Errorf("触发器所属表修复记录不唯一")
		}
		found = true
		object.ColumnMapping = table.ColumnMapping
		if !slices.Contains(object.Dependencies, table.ID) {
			object.Dependencies = append(object.Dependencies, table.ID)
		}
	}
	return nil
}
