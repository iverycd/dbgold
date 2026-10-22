package datamigrate

import (
	"context"
	"fmt"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
)

func NativeRepairHasFinalState(object repair.Object) bool {
	if object.Native == nil {
		return false
	}
	for i, unit := range object.Native.Units {
		if (unit.Phase == "event_activate" || unit.Phase == "mv_restore") && i < len(object.Units) && object.Units[i].Status != "succeeded" {
			return true
		}
	}
	return false
}

// NativeRepairFinalizationReady is shared by DDL retry and attached recovery.
// Final-state units may all be pending at once; every other object/data unit
// must be complete. The executing object's ordinary steps run before the final
// read-only proof, so they are excluded here and enforced by its writer.
func NativeRepairFinalizationReady(objects []repair.Object, currentID string) error {
	for _, object := range objects {
		if object.Native == nil || object.Native.Managed || object.DiagnosticOnly || object.ID == currentID {
			continue
		}
		if !object.TrustedNative() {
			return fmt.Errorf("对象 %s 的原生冻结计划不可信", object.Name)
		}
		if object.DataStatus != "succeeded" && object.DataStatus != "not_required" {
			return fmt.Errorf("表 %s 的数据尚未完成，自动执行策略保持暂停", object.Name)
		}
		for i, unit := range object.Units {
			if i < len(object.Native.Units) {
				phase := object.Native.Units[i].Phase
				if phase == "event_activate" || phase == "mv_restore" {
					continue
				}
			}
			if unit.Status != "succeeded" {
				return fmt.Errorf("对象 %s 的冻结步骤尚未完成，自动执行策略保持暂停", object.Name)
			}
		}
	}
	return nil
}

// VerifyNativeRepairFinalization repeats whole-schema metadata and configured
// full-content proof after a repair. An earlier failed job report is not proof
// that newly recovered data now matches the source.
func VerifyNativeRepairFinalization(ctx context.Context, r source.Reader, w target.Writer, cfg Config, objects []repair.Object) error {
	nr, ok := r.(damengnative.Reader)
	if !ok {
		return fmt.Errorf("自动执行策略恢复需要原始达梦源连接")
	}
	nw, ok := w.(damengnative.Writer)
	if !ok {
		return fmt.Errorf("目标不支持原生完整核验")
	}
	snapshot, err := nr.SnapshotDamengNative(ctx)
	if err != nil || snapshot == nil {
		return fmt.Errorf("重新读取源完整目录失败: %v", err)
	}
	if damengnative.HasErrors(snapshot.Issues) {
		return fmt.Errorf("源完整目录仍有阻断问题")
	}
	destination, err := nw.InspectDamengNative(ctx)
	if err != nil {
		return err
	}
	if damengnative.HasErrors(destination.Issues) {
		return fmt.Errorf("目标事件触发器或原生目录未处于可恢复状态")
	}
	if snapshot.Profile.Version != destination.Profile.Version || damengnative.Fingerprint(snapshot.Profile.Settings) != damengnative.Fingerprint(destination.Profile.Settings) {
		return fmt.Errorf("源目标版本或语义参数已改变")
	}
	frozen := map[string]repair.Object{}
	for _, object := range objects {
		if object.Native != nil && object.Native.Kind != "native_drop" {
			frozen[object.ID] = object
		}
	}
	for _, object := range snapshot.Objects {
		original, exists := frozen[object.ID]
		trusted := original.TrustedNative()
		if original.Native != nil && original.Native.Managed {
			trusted = original.DiagnosticOnly && original.NativeFingerprint == damengnative.Fingerprint(original.Native) && original.Schema == original.Native.Schema && original.TargetName == original.Native.Name
		}
		if !exists || !trusted {
			return fmt.Errorf("源对象 %s 已偏离原生冻结清单", object.Name)
		}
		for _, unit := range object.Units {
			mapped, _, rewriteErr := damengnative.RewriteUnit(unit, snapshot.Profile.Schema, destination.Profile.Schema)
			if unit.Phase == "mv_history" && object.Materialized != nil && object.Materialized.History != nil {
				plan, mapErr := damengnative.RewriteMaterializedHistory(object.Materialized.History, snapshot.Profile.Schema, destination.Profile.Schema)
				rewriteErr = mapErr
				if mapErr == nil {
					mapped = damengnative.Unit{SQL: damengnative.MaterializedHistorySQL(destination.Profile.Schema, object.Name, plan), Phase: unit.Phase}
				}
			}
			if unit.Phase == "event_activate" {
				mapped = damengnative.Unit{SQL: damengnative.EventTriggerStateSQL(destination.Profile.Schema, object.Name, object.Status == "ENABLED"), Phase: unit.Phase}
				rewriteErr = nil
			}
			if rewriteErr != nil {
				return rewriteErr
			}
			if object.EventTrigger && unit.Phase == "event_create" {
				mapped.SQL, rewriteErr = damengnative.DisabledEventTriggerDDL(mapped.SQL)
				if rewriteErr != nil {
					return rewriteErr
				}
			}
			matched := false
			for _, expected := range original.Native.Units {
				expectedSQL := expected.SQL
				if object.EventTrigger && unit.Phase == "event_create" && expected.Phase == "event_create" {
					expectedSQL, rewriteErr = damengnative.DisabledEventTriggerDDL(expectedSQL)
					if rewriteErr != nil {
						return rewriteErr
					}
				}
				if expectedSQL == mapped.SQL && expected.SourceSQLHash == mapped.SourceSQLHash {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("源对象 %s 的定义或状态已偏离原生冻结清单", object.Name)
			}
		}
		delete(frozen, object.ID)
	}
	if len(frozen) > 0 {
		return fmt.Errorf("原生冻结对象已从源目录消失")
	}
	expected := *snapshot
	expected.Objects = append([]damengnative.Object{}, snapshot.Objects...)
	for i := range expected.Objects {
		if expected.Objects[i].EventTrigger {
			// Every imported event must still be disabled at the proof point.
			expected.Objects[i].Status = "DISABLED"
		}
	}
	issues, err := nw.VerifyDamengNative(ctx, &expected)
	if err != nil {
		return err
	}
	if damengnative.HasErrors(issues) {
		return fmt.Errorf("目标完整目录或行数仍未通过原生核验: %+v", issues)
	}
	if !cfg.ValidateData {
		return nil
	}
	cfg.Mode, cfg.Content, cfg.TargetSchema = "all", "both", destination.Profile.Schema
	log := make(chan string, 256)
	go func() {
		for range log {
		}
	}()
	defer close(log)
	m := NewMigrator(r, w, &Job{LogCh: log}, cfg)
	m.repairs = &repairRecorder{objects: objects}
	var tables []string
	for _, table := range snapshot.Tables {
		if table.Shadow {
			continue
		}
		tables = append(tables, nativeSourceTable(table))
	}
	m.freezeDataValidation(ctx, tables)
	for i := range m.validationTables {
		for _, table := range snapshot.Tables {
			if table.Shadow {
				continue
			}
			if m.validationTables[i].name == nativeSourceTable(table) {
				m.validationTables[i].target = table.Name
				m.markDataValidationCopied(m.validationTables[i].name)
			}
		}
	}
	var mappedObjects []damengnative.Object
	for _, object := range objects {
		if object.Native != nil {
			mappedObjects = append(mappedObjects, *object.Native)
		}
	}
	m.markNativeRowIDValidation(&damengnative.Preflight{Snapshot: snapshot, Objects: mappedObjects}, map[string]error{})
	report := MigrationReport{DataValidation: &validation.Report{Status: validation.StatusIncomplete}}
	m.validateDataContent(ctx, &report)
	if report.DataValidation.HasFailures() {
		return fmt.Errorf("整任务内容核验尚未通过: %s", report.DataValidation.Status)
	}
	return nil
}
