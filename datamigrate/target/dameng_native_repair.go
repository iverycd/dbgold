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
		if err := validateDamengNativeSQL(w.schema, out[i].SQL); err != nil {
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
		out[i].Status, out[i].Error = "running", ""
		if err := persist(); err != nil {
			out[i].Status = "pending"
			return out, fmt.Errorf("保存原生执行前状态失败: %w", err)
		}
		err := w.ExecuteDamengNative(ctx, out[i].SQL)
		if err != nil {
			out[i].Status, out[i].Error = repairErrorStatus(err), err.Error()
			return out, errors.Join(err, persist())
		}
		out[i].Status, out[i].Error, out[i].Selected = "succeeded", "", false
		if err = persist(); err != nil {
			out[i].Status, out[i].Error = "unknown", err.Error()
			return out, fmt.Errorf("原生 SQL 已执行但结果保存失败: %w", err)
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
	for _, candidate := range found.Units {
		if strings.TrimSpace(candidate.SQL) == query {
			return true, false, nil
		}
	}
	return false, false, nil
}
