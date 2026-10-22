package handler

import (
	"context"
	"fmt"
	"slices"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

// Phase selection changes only review flags. SQL, unit order and the native
// fingerprint remain immutable, and successful units can never be replayed.
func nativeRecoveryPhaseUnits(object repair.Object, phase string) []repair.Unit {
	units := slices.Clone(object.Units)
	if object.Native == nil {
		if phase != "install" {
			for i := range units {
				units[i].Selected = false
			}
		}
		return units
	}
	for i := range units {
		if i >= len(object.Native.Units) {
			units[i].Selected = false
			continue
		}
		kind := object.Native.Units[i].Phase
		final := kind == "mv_restore" || kind == "event_activate"
		selected := !final
		if phase != "install" {
			selected = kind == phase
		}
		units[i].Selected = units[i].Selected && units[i].Status != "succeeded" && selected
	}
	return units
}

func nativeRecoveryMergeSelections(original repair.Object, units []repair.Unit) []repair.Unit {
	merged := slices.Clone(units)
	for i := range merged {
		if merged[i].Status == "succeeded" {
			merged[i].Selected = false
		} else if i < len(original.Units) {
			merged[i].Selected = original.Units[i].Selected
		}
	}
	return merged
}

func nativeRecoveryHasSelected(units []repair.Unit) bool {
	for _, unit := range units {
		if unit.Selected && unit.Status != "succeeded" {
			return true
		}
	}
	return false
}
func nativeRecoveryInstallationComplete(object repair.Object) bool {
	if object.Native == nil {
		return object.Status == "succeeded"
	}
	for i, unit := range object.Units {
		if i >= len(object.Native.Units) {
			return false
		}
		phase := object.Native.Units[i].Phase
		if phase != "mv_restore" && phase != "event_activate" && unit.Status != "succeeded" {
			return false
		}
	}
	return true
}

func nativeRecoveryCompleteManaged(objects []repair.Object, completed map[string]bool) {
	for changed := true; changed; {
		changed = false
		for _, object := range objects {
			if object.Native == nil || !object.Native.Managed || completed[object.ID] || !completed[object.Native.ParentID] {
				continue
			}
			completed[object.ID] = true
			changed = true
		}
	}
}

type nativeRecoveryFinalization struct {
	execute   func(context.Context, repair.Object, []repair.Unit, func([]repair.Unit) error) ([]repair.Unit, error)
	proof     func(context.Context) error
	pinEvents func(context.Context, func(context.Context) error) error
	progress  func(repair.Object, []repair.Unit) error
	save      func(repair.Object) error
}

// The proof is executed once for the entire recovered job. Every MV policy is
// restored and read back before opening the one final event session. After the
// first event activation there are no catalog/content reads or new logins.
func executeNativeRecoveryFinalStates(ctx context.Context, items []repair.Object, run nativeRecoveryFinalization) ([]repair.Object, error) {
	items = slices.Clone(items)
	needed := false
	for _, object := range items {
		if nativeRecoveryHasSelected(nativeRecoveryPhaseUnits(object, "mv_restore")) || nativeRecoveryHasSelected(nativeRecoveryPhaseUnits(object, "event_activate")) {
			needed = true
			break
		}
	}
	if !needed {
		return items, nil
	}
	if run.proof == nil {
		return items, fmt.Errorf("原生接续缺少整任务最终核验")
	}
	if err := run.proof(ctx); err != nil {
		return items, err
	}
	// target.ExecuteRepair requires the same proof callback. It must not repeat
	// SQL reads after another object's event trigger has already been enabled.
	finalContext := damengnative.WithFinalizationVerifier(ctx, func(context.Context) error { return nil })
	phase := func(current context.Context, want string) error {
		for i, object := range items {
			if object.Native == nil {
				continue
			}
			units := nativeRecoveryPhaseUnits(object, want)
			if !nativeRecoveryHasSelected(units) {
				continue
			}
			out, err := run.execute(current, object, units, func(progress []repair.Unit) error {
				if run.progress == nil {
					return nil
				}
				return run.progress(object, nativeRecoveryMergeSelections(object, progress))
			})
			object.Units = nativeRecoveryMergeSelections(object, out)
			object.Status, object.Error = "pending", ""
			if object.AllUnitsSucceeded() {
				object.Status = "succeeded"
				object.Verification = nil
				object.ManualVerification = nil
			}
			if err != nil {
				object.Status, object.Error = "failed", err.Error()
				for _, unit := range object.Units {
					if unit.Status == "unknown" || unit.Status == "running" {
						object.Status = "unknown"
					}
				}
			}
			items[i] = object
			if run.save != nil {
				if saveErr := run.save(object); saveErr != nil {
					return saveErr
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := phase(finalContext, "mv_restore"); err != nil {
		return items, err
	}
	hasEvents := false
	for _, object := range items {
		if nativeRecoveryHasSelected(nativeRecoveryPhaseUnits(object, "event_activate")) {
			hasEvents = true
			break
		}
	}
	if hasEvents {
		if run.pinEvents == nil {
			return items, fmt.Errorf("原生接续缺少最终固定事件会话")
		}
		if err := run.pinEvents(finalContext, func(final context.Context) error { return phase(final, "event_activate") }); err != nil {
			return items, err
		}
	}
	return items, nil
}
