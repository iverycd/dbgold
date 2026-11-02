package datamigrate

import (
	"context"
	"errors"
	"fmt"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

// Event activation is deliberately outside runDamengNative: Run may still
// perform a full target data scan after that function returns. No target SQL
// may follow activation; persisting the completed report remains local only.
func (m *Migrator) finalizeDamengNativeEvents(ctx context.Context, report *MigrationReport) {
	if report.NativeSchema == nil || report.NativeSchema.pendingPlan == nil {
		return
	}
	native := report.NativeSchema
	p, states := native.pendingPlan, native.pendingStates
	native.pendingPlan, native.pendingStates = nil, nil
	defer func() {
		m.collectNativeResults(report, p, states)
		if !report.HasFailures() {
			m.logMigrationCompletion("达梦 Schema 原生迁移结束，共 %d 个对象、%d 张数据表", len(p.Objects), len(p.Snapshot.Tables))
		}
	}()
	var events, policies []damengnative.Object
	for _, object := range p.Objects {
		if object.EventTrigger && !object.Managed {
			events = append(events, object)
		}
		for _, unit := range object.Units {
			if unit.Phase == "mv_restore" {
				policies = append(policies, object)
				break
			}
		}
	}
	if len(events) == 0 && len(policies) == 0 {
		return
	}
	if ctx.Err() != nil || m.nativePersistenceFailed() {
		return
	}
	w := m.writer.(damengnative.Writer)
	blockNativeHistoryGroups(p, states)
	for _, object := range policies {
		members := nativePolicyMembers(p, object)
		if !m.nativeScopeReady(p.Objects, members, states) || !nativePolicyValidated(report, p, members) {
			if states[object.ID] == nil {
				states[object.ID] = nativeObjectFailure("dependency", fmt.Errorf("物化视图依赖组未完成核验，自动刷新保持暂停"))
			}
			continue
		}
		if err := m.executeNativePhase(ctx, w, []damengnative.Object{object}, "mv_restore", states); err != nil {
			native.Verification = append(native.Verification, nativeFailure("物化视图刷新策略", err))
			return
		}
		if states[object.ID] != nil {
			continue
		}
		verifier, ok := w.(interface {
			VerifyDamengNativeMaterializedPolicy(context.Context, damengnative.Object) error
		})
		var err error
		if !ok {
			err = fmt.Errorf("写入器缺少原始刷新策略核验能力")
		} else {
			err = verifier.VerifyDamengNativeMaterializedPolicy(ctx, object)
		}
		if err != nil {
			err = errors.Join(err, m.markNativePolicyProofFailure(object.ID, err))
			states[object.ID] = nativeObjectFailure("execute", err)
			native.Verification = append(native.Verification, damengnative.Issue{ObjectID: object.ID, Kind: object.Kind, Name: object.Name, Severity: "error", Reason: err.Error()})
			if m.stopMigrationTask(ctx, err) {
				return
			}
		}
	}
	if len(events) == 0 {
		return
	}
	if report.HasFailures() || len(states) > 0 || !m.nativeReadyForEventActivation() {
		native.Verification = append(native.Verification, nativeFailure("自动执行策略保持暂停", fmt.Errorf("迁移、对象核验或内容校验未全部完成，未恢复事件触发器启停状态")))
		return
	}
	finalizer, ok := w.(interface {
		WithDamengNativeEventFinalization(context.Context, func(context.Context) error) error
	})
	if !ok {
		native.Verification = append(native.Verification, nativeFailure("事件触发器最终状态", fmt.Errorf("写入器缺少最终固定会话")))
		return
	}
	err := finalizer.WithDamengNativeEventFinalization(ctx, func(final context.Context) error {
		for _, object := range events {
			for _, unit := range object.Units {
				if unit.Phase != "event_activate" {
					continue
				}
				if err := m.executeNativeSavedUnit(final, w, object.ID, unit.SQL); err != nil {
					states[object.ID] = err
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		var failure *damengnative.EventFinalizationFailure
		if errors.As(err, &failure) && m.repairs != nil {
			m.repairs.mu.Lock()
			for i := range m.repairs.objects {
				object := &m.repairs.objects[i]
				if repair.ApplyNativeEventRecovery(object, failure.States) {
					states[object.ID] = err
					err = errors.Join(err, m.flushRepairObjectLocked(*object))
				}
			}
			m.repairs.mu.Unlock()
		}
		native.Verification = append(native.Verification, nativeFailure("事件触发器最终状态", err))
		return
	}
	for _, object := range events {
		if object.Status == "ENABLED" {
			native.Verification = append(native.Verification, damengnative.Issue{ObjectID: object.ID, Kind: "trigger", Name: object.Name, Severity: "warning", Reason: "事件触发器已在全部目标核验完成后恢复启用；导入停写窗口结束，最后连接退出可执行源端定义的 LOGOUT 逻辑，之后新连接及数据库事件按原定义运行"})
			break
		}
	}
}

func (m *Migrator) markNativePolicyProofFailure(id string, cause error) error {
	if m.repairs == nil {
		return nil
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	for i := range m.repairs.objects {
		object := &m.repairs.objects[i]
		if object.ID != id || object.Native == nil {
			continue
		}
		for j, unit := range object.Native.Units {
			if unit.Phase == "mv_restore" && j < len(object.Units) {
				object.Units[j].Status, object.Units[j].Error, object.Units[j].Selected = "unknown", cause.Error(), false
			}
		}
		object.Status, object.Error = "unknown", cause.Error()
		return m.flushRepairObjectLocked(*object)
	}
	return nil
}

func (m *Migrator) nativeReadyForEventActivation() bool {
	if m.repairs == nil {
		return false
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	if m.repairs.sinkErr != nil {
		return false
	}
	for _, object := range m.repairs.objects {
		if object.DiagnosticOnly {
			continue
		}
		if object.DataStatus != "succeeded" && object.DataStatus != "not_required" {
			return false
		}
		for i, unit := range object.Units {
			if object.Native != nil && i < len(object.Native.Units) && (object.Native.Units[i].Phase == "event_activate" || object.Native.Units[i].Phase == "mv_restore") {
				continue
			}
			if unit.Status != "succeeded" {
				return false
			}
		}
	}
	return true
}
