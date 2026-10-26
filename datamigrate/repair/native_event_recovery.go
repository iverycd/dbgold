package repair

import (
	"strings"

	"dbgold/datamigrate/damengnative"
)

// ApplyNativeEventRecovery records a measured policy rollback. Re-activating a
// confirmed disabled hook is not replaying its successful CREATE; the immutable
// native definition and every non-policy step are deliberately untouched.
func ApplyNativeEventRecovery(object *Object, states []damengnative.EventStateRecovery) bool {
	if object == nil || !object.TrustedNative() || !object.Native.EventTrigger {
		return false
	}
	changed := false
	for _, state := range states {
		if state.Schema != object.Schema || state.Name != object.TargetName || state.Status != "pending" && state.Status != "unknown" && state.Status != "succeeded" {
			continue
		}
		for i, unit := range object.Native.Units {
			if i >= len(object.Units) || unit.Phase != "event_activate" || unit.SQL != state.SQL {
				continue
			}
			if state.Status == "succeeded" {
				// A read-only DISABLED proof may finish an uncertain original
				// disable policy before any other event is enabled. It can never
				// prove an ENABLED policy or an absent/unverified CREATE.
				switch strings.ToUpper(strings.TrimSpace(object.Native.Status)) {
				case "DISABLED", "DISABLE", "N", "0":
				default:
					continue
				}
				if unit.SQL != damengnative.EventTriggerStateSQL(object.Schema, object.TargetName, false) {
					continue
				}
			}
			out := &object.Units[i]
			out.Status, out.Selected = state.Status, state.Status != "succeeded"
			out.Error = "本轮最终启用未全部完成，事件已暂停，需重新核验后恢复"
			if state.Status == "unknown" {
				out.Error = "最终启用失败，事件停用结果尚未确认: " + state.Error
			}
			object.Status, object.Error = out.Status, out.Error
			if state.Status == "succeeded" {
				out.Error, object.Error, object.Status = "", "", "pending"
				if object.AllUnitsSucceeded() {
					object.Status = "succeeded"
				}
			}
			changed = true
		}
	}
	return changed
}
