package handler

import (
	"context"
	"fmt"

	"dbgold/datamigrate"
	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

func nativeRepairHasPendingOrdinaryUnits(object repair.Object) bool {
	if object.Native == nil {
		return false
	}
	for i, unit := range object.Units {
		if unit.Status == "succeeded" || i >= len(object.Native.Units) {
			continue
		}
		phase := object.Native.Units[i].Phase
		if phase != "event_activate" && phase != "mv_restore" {
			return true
		}
	}
	return false
}

func checkNativeRepairEventSafety(ctx context.Context, w target.Writer, object repair.Object) error {
	if !nativeRepairHasPendingOrdinaryUnits(object) {
		return nil
	}
	native, ok := w.(damengnative.Writer)
	if !ok {
		return fmt.Errorf("原生修复缺少目标目录检查器")
	}
	catalog, err := native.InspectDamengNative(ctx)
	if err != nil {
		return err
	}
	if damengnative.HasErrors(catalog.Issues) {
		return fmt.Errorf("目标存在启用的事件触发器或目录阻断，未执行原生修复 DDL")
	}
	return nil
}

func nativeRepairFinalizationContext(ctx context.Context, jobID, currentID string, r source.Reader, w target.Writer, cfg datamigrate.Config) (context.Context, error) {
	objects, _, err := repairObjects(jobID)
	if err != nil {
		return ctx, err
	}
	if err := datamigrate.NativeRepairFinalizationReady(objects, currentID); err != nil {
		return ctx, err
	}
	return damengnative.WithFinalizationVerifier(ctx, func(proof context.Context) error {
		objects, _, err := repairObjects(jobID)
		if err != nil {
			return err
		}
		if err := datamigrate.NativeRepairFinalizationReady(objects, currentID); err != nil {
			return err
		}
		return datamigrate.VerifyNativeRepairFinalization(proof, r, w, cfg, objects)
	}), nil
}
