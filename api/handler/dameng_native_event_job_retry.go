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

// Retrying an event's final state is a job-wide policy operation. A process
// crash can leave another imported event enabled even though this object's
// activation failed. Pause only authenticated frozen hooks, complete the
// selected object's ordinary steps, prove the whole job, then restore all
// pending policies in the same final event session.
func retryNativeEventJob(ctx context.Context, jobID string, current repair.Object, r source.Reader, w target.Writer, cfg datamigrate.Config, progress func([]repair.Unit) error) ([]repair.Unit, error) {
	objects, _, err := repairObjects(jobID)
	if err != nil {
		return current.Units, err
	}
	currentIndex := -1
	for i := range objects {
		if objects[i].ID == current.ID {
			objects[i] = current
			currentIndex = i
		}
	}
	if currentIndex < 0 || !nativeEventRepairObject(current) {
		return current.Units, fmt.Errorf("事件对象不在完整冻结账本中")
	}
	save := func(object repair.Object) error {
		for i := range objects {
			if objects[i].ID == object.ID {
				objects[i] = object
				break
			}
		}
		if object.ID == current.ID && progress != nil {
			if err := progress(object.Units); err != nil {
				return err
			}
		}
		return persistMigrationRepairObject(jobID, object)
	}
	err = pauseNativeFrozenEvents(ctx, w, objects, save)
	if err != nil {
		return objects[currentIndex].Units, err
	}
	current = objects[currentIndex]
	if err = checkNativeRepairEventSafety(ctx, w, current); err != nil {
		return current.Units, err
	}
	install := nativeRecoveryPhaseUnits(current, "install")
	if nativeRecoveryHasSelected(install) {
		units, installErr := target.ExecuteRepair(ctx, w, current, install, func(units []repair.Unit) error {
			object := current
			object.Units = nativeRecoveryMergeSelections(current, units)
			return save(object)
		})
		current.Units = nativeRecoveryMergeSelections(current, units)
		current.Status = "pending"
		if saveErr := save(current); saveErr != nil {
			return current.Units, saveErr
		}
		if installErr != nil {
			return current.Units, installErr
		}
	}
	// Whole-job policies are immutable and have no editable SQL. Explicitly
	// select only their incomplete final states; successful ordinary units are
	// never replayed, including the CREATE that originally installed each hook.
	selectNativeFinalPolicies(objects)
	finalizer, ok := w.(interface {
		WithDamengNativeEventFinalization(context.Context, func(context.Context) error) error
	})
	if !ok {
		return objects[currentIndex].Units, fmt.Errorf("目标缺少事件最终固定会话")
	}
	final, err := executeNativeRecoveryFinalStates(ctx, objects, nativeRecoveryFinalization{
		forceProof: true,
		execute: func(run context.Context, object repair.Object, units []repair.Unit, step func([]repair.Unit) error) ([]repair.Unit, error) {
			return target.ExecuteRepair(run, w, object, units, step)
		},
		proof: func(proof context.Context) error {
			if err := datamigrate.NativeRepairFinalizationReady(objects, ""); err != nil {
				return err
			}
			return datamigrate.VerifyNativeRepairFinalization(proof, r, w, cfg, objects)
		},
		pinEvents: finalizer.WithDamengNativeEventFinalization,
		progress:  func(object repair.Object, units []repair.Unit) error { object.Units = units; return save(object) },
		save:      save,
	})
	for _, object := range final {
		if object.ID == current.ID {
			return object.Units, err
		}
	}
	return objects[currentIndex].Units, err
}

// The attached-data recovery route can enter finalization with only a subset
// of event objects selected. Reload the entire frozen job so an uncertain
// DISABLED policy cannot be reconciled after another hook has been enabled.
func prepareNativeEventJobPolicies(ctx context.Context, jobID string, w target.Writer) ([]repair.Object, error) {
	objects, _, err := repairObjects(jobID)
	if err != nil {
		return nil, err
	}
	err = pauseNativeFrozenEvents(ctx, w, objects, func(object repair.Object) error { return persistMigrationRepairObject(jobID, object) })
	selectNativeFinalPolicies(objects)
	return objects, err
}

func pauseNativeFrozenEvents(ctx context.Context, w target.Writer, objects []repair.Object, save func(repair.Object) error) error {
	var events []damengnative.Object
	for _, object := range objects {
		if !nativeEventRepairObject(object) {
			continue
		}
		if !object.TrustedNative() {
			return fmt.Errorf("事件 %s 的冻结恢复计划不可信", object.Name)
		}
		events = append(events, *object.Native)
	}
	if len(events) == 0 {
		return nil
	}
	pause, ok := w.(interface {
		PauseDamengNativeEventRecovery(context.Context, []damengnative.Object, func(damengnative.EventStateRecovery) error) error
	})
	if !ok {
		return fmt.Errorf("目标缺少原生事件整体暂停恢复能力")
	}
	return pause.PauseDamengNativeEventRecovery(ctx, events, func(state damengnative.EventStateRecovery) error {
		for i := range objects {
			if repair.ApplyNativeEventRecovery(&objects[i], []damengnative.EventStateRecovery{state}) {
				return save(objects[i])
			}
		}
		return fmt.Errorf("事件暂停结果不属于可信冻结账本")
	})
}

func nativeEventRepairObject(object repair.Object) bool {
	return object.Native != nil && object.Native.Kind == "trigger" && object.Native.EventTrigger && !object.Native.Managed
}

func selectNativeFinalPolicies(objects []repair.Object) {
	for i := range objects {
		for j := range objects[i].Units {
			unit := &objects[i].Units[j]
			unit.Selected = false
			if objects[i].Native != nil && j < len(objects[i].Native.Units) {
				phase := objects[i].Native.Units[j].Phase
				unit.Selected = unit.Status != "succeeded" && (phase == "event_activate" || phase == "mv_restore")
			}
		}
	}
}
