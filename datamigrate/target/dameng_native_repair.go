package target

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

func (w *DaMengNativeWriter) executeNativeRepair(ctx context.Context, obj repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	check := obj
	check.Units = out
	if !check.TrustedNative() || obj.Schema != w.schema {
		return out, fmt.Errorf("达梦原生修复计划已改变或不属于目标 schema")
	}
	selected := []int{}
	for i := range out {
		if out[i].Status == "succeeded" {
			out[i].Selected = false
			continue
		}
		if !out[i].Selected {
			continue
		}
		if err := validateNativeObjectUnit(w.schema, *obj.Native, obj.Native.Units[i]); err != nil {
			return out, err
		}
		selected = append(selected, i)
	}
	if len(selected) == 0 {
		return out, fmt.Errorf("未选择原生修复步骤")
	}
	persist := func() error {
		if onStep == nil {
			return nil
		}
		return onStep(slices.Clone(out))
	}
	// Reconcile every uncertain result before restoring any event state: reads
	// must not open another login session after the first activation.
	for _, i := range selected {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if out[i].Status == "unknown" || out[i].Status == "running" {
			done, retry, err := w.reconcileNativeUnit(ctx, *obj.Native, i)
			if err != nil {
				return out, err
			}
			if done {
				out[i].Status, out[i].Error, out[i].Selected = "succeeded", "", false
				if err = persist(); err != nil {
					return out, err
				}
				continue
			}
			if !retry {
				return out, fmt.Errorf("原生对象 %s 的执行结果不确定，目录尚不能证明可安全重试", obj.TargetName)
			}
		}
	}
	execute := func(run context.Context, i int) error {
		if out[i].Status == "succeeded" {
			return nil
		}
		if err := run.Err(); err != nil {
			return err
		}
		out[i].Status, out[i].Error = "running", ""
		if err := persist(); err != nil {
			out[i].Status = "pending"
			return fmt.Errorf("保存原生执行前状态失败: %w", err)
		}
		err := damengnative.ExecuteUnit(run, w, *obj.Native, obj.Native.Units[i])
		if err != nil {
			out[i].Status, out[i].Error = repairErrorStatus(err), err.Error()
			return errors.Join(err, persist())
		}
		out[i].Status, out[i].Error, out[i].Selected = "succeeded", "", false
		if err = persist(); err != nil {
			out[i].Status, out[i].Error = "unknown", err.Error()
			return fmt.Errorf("原生 SQL 已执行但结果保存失败: %w", err)
		}
		return nil
	}
	var activate, policies []int
	for _, i := range selected {
		if out[i].Status == "succeeded" {
			continue
		}
		if obj.Native.Units[i].Phase == "event_activate" {
			activate = append(activate, i)
			continue
		}
		if obj.Native.Units[i].Phase == "mv_restore" {
			policies = append(policies, i)
			continue
		}
		if err := execute(ctx, i); err != nil {
			return out, err
		}
	}
	if len(activate) > 0 || len(policies) > 0 {
		for i, unit := range out {
			phase := obj.Native.Units[i].Phase
			if phase != "event_activate" && phase != "mv_restore" && unit.Status != "succeeded" {
				return out, fmt.Errorf("其他冻结步骤尚未完成，自动执行策略保持暂停")
			}
		}
		if err := damengnative.VerifyFinalization(ctx); err != nil {
			return out, err
		}
	}
	for _, i := range policies {
		if err := execute(ctx, i); err != nil {
			return out, err
		}
		if err := w.VerifyDamengNativeMaterializedPolicy(ctx, *obj.Native); err != nil {
			out[i].Status, out[i].Error, out[i].Selected = "unknown", err.Error(), false
			return out, errors.Join(err, persist())
		}
	}
	if len(activate) > 0 {
		for i, u := range out {
			if obj.Native.Units[i].Phase != "event_activate" && u.Status != "succeeded" {
				return out, fmt.Errorf("事件触发器其他冻结步骤尚未完成，保持禁用")
			}
		}
		err := w.WithDamengNativeEventFinalization(ctx, func(final context.Context) error {
			for _, i := range activate {
				if err := execute(final, i); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// Presence alone never proves a failed CREATE completed: read the definition
// back, and require an exact immutable unit before reconciling success.
func (w *DaMengNativeWriter) reconcileNativeUnit(ctx context.Context, object damengnative.Object, unit int) (done, retry bool, err error) {
	snapshot, err := damengnative.ReadCatalog(ctx, w.db, w.schema, true)
	if err != nil {
		return false, false, err
	}
	var found *damengnative.Object
	for i := range snapshot.Objects {
		candidate := &snapshot.Objects[i]
		if candidate.Kind == object.Kind && candidate.Name == object.Name {
			found = candidate
			break
		}
	}
	query := strings.TrimSpace(object.Units[unit].SQL)
	upper := strings.ToUpper(query)
	if strings.HasPrefix(upper, "DROP ") {
		return found == nil, found != nil, nil
	}
	if found == nil {
		return false, strings.HasPrefix(upper, "CREATE "), nil
	}
	if object.EventTrigger && object.Units[unit].Phase == "event_activate" {
		return found.Status == object.Status, found.Status == "DISABLED" || found.Status == "ENABLED", nil
	}
	if object.Units[unit].Phase == "mv_restore" {
		if err := w.VerifyDamengNativeMaterializedPolicy(ctx, object); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	expected := object.Units[unit]
	if expected.SourceSQLHash != "" {
		// A durable ledger cannot carry credentials. Re-read and authenticate
		// the original source definition before proving a sensitive CREATE.
		actual, e := damengnative.ExecutionSQL(ctx, object, expected)
		if e != nil {
			return false, false, e
		}
		expected, e = damengnative.FreezeLinkUnit(actual, expected.Phase)
		if e != nil {
			return false, false, e
		}
	}
	for _, candidate := range found.Units {
		if strings.TrimSpace(candidate.SQL) == query && (expected.SourceSQLHash == "" || damengnative.LinkUnitsEquivalent(expected, candidate)) {
			if object.EventTrigger && object.Units[unit].Phase == "event_create" && found.Status != "DISABLED" {
				return false, false, nil
			}
			return true, false, nil
		}
	}
	return false, false, nil
}
