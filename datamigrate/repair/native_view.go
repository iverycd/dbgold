package repair

import (
	"fmt"
	"slices"

	"dbgold/datamigrate/damengnative"
)

// Original is the source-mapped plan saved before the first manual revision.
// InstalledSQL is the last confirmed installed definition, if any.
type NativeViewDraft struct {
	Original            damengnative.Object `json:"original"`
	OriginalFingerprint string              `json:"original_fingerprint"`
	InstalledSQL        string              `json:"installed_sql,omitempty"`
}

func (o Object) nativeViewIdentity() bool {
	n := o.Native
	if n == nil || n.Managed || n.Materialized != nil || o.DiagnosticOnly || o.Kind != "view" || n.Kind != o.Kind {
		return false
	}
	return o.ID != "" && n.ID == o.ID && o.Schema != "" && n.Schema == o.Schema &&
		o.TargetName != "" && n.Name == o.TargetName && slices.Equal(o.Dependencies, n.Dependencies)
}

func (o Object) trustedViewRevision() bool {
	p := o.NativeViewDraft
	if !o.nativeViewIdentity() || p.OriginalFingerprint != damengnative.Fingerprint(p.Original) || len(o.Native.Units) != 2 || len(o.Units) != 2 {
		return false
	}
	expected := p.Original
	expected.Units = o.Native.Units
	expected.CompileSQL = "ALTER VIEW " + damengnative.Qualified(o.Schema, o.TargetName) + " COMPILE"
	_, err := damengnative.ViewRepairDefinition(o.Native.Units[0].SQL, o.Schema, o.TargetName)
	return err == nil && damengnative.Fingerprint(expected) == o.NativeFingerprint &&
		o.Native.Units[0].Phase == "after_data" && o.Native.Units[1].Phase == "compile" &&
		o.Native.Units[1].SQL == expected.CompileSQL
}

func (o Object) CanEditNativeView() bool {
	if !o.nativeViewIdentity() || !slices.Contains([]string{"failed", "blocked", "draft", "cancelled"}, o.Status) || o.NativeFingerprint != damengnative.Fingerprint(o.Native) || len(o.Units) != len(o.Native.Units) {
		return false
	}
	if o.NativeViewDraft != nil && !o.TrustedNative() {
		return false
	}
	for i, u := range o.Units {
		if u.SQL != o.Native.Units[i].SQL || u.OriginalSQL != u.SQL || u.Destructive || slices.Contains([]string{"queued", "running", "unknown", "unverified", "needs_verification"}, u.Status) {
			return false
		}
		if o.Native.Units[i].Phase != "compile" && (i != 0 || o.Native.Units[i].Phase != "after_data") {
			return false
		}
	}
	return true
}

func (o Object) WithNativeViewDraft(sql string, reviewed bool) (Object, error) {
	if !o.CanEditNativeView() {
		return o, fmt.Errorf("当前达梦普通视图的身份或执行状态不支持编辑")
	}
	sql = damengnative.StripClientDelimiter(sql)
	if _, err := damengnative.ViewRepairDefinition(sql, o.Schema, o.TargetName); err != nil {
		return o, err
	}
	plan := *o.Native
	draft := NativeViewDraft{Original: plan, OriginalFingerprint: o.NativeFingerprint}
	if o.NativeViewDraft != nil {
		draft = *o.NativeViewDraft
	}
	compile := "ALTER VIEW " + damengnative.Qualified(o.Schema, o.TargetName) + " COMPILE"
	plan.CompileSQL = compile
	plan.Units = []damengnative.Unit{{SQL: sql, Phase: "after_data"}, {SQL: compile, Phase: "compile"}}
	units := []Unit{
		{ID: "view-create", SQL: sql, OriginalSQL: sql, Status: "pending", Selected: true},
		{ID: "view-compile", SQL: compile, OriginalSQL: compile, Status: "pending", Selected: true},
	}
	if len(o.Units) > 0 && o.Native.Units[0].Phase == "after_data" && o.Units[0].Status == "succeeded" {
		draft.InstalledSQL = o.Units[0].SQL
		if sql == o.Units[0].SQL {
			units[0] = o.Units[0]
			units[0].Selected = false
			if len(o.Units) == 2 && o.Units[1].SQL == compile && o.Units[1].Status != "succeeded" {
				units[1] = o.Units[1]
				units[1].Selected = true
			}
		}
	}
	// A previously failed validation may have both physical steps completed.
	// Recheck the saved CREATE and compile again before declaring success.
	o.Native, o.NativeFingerprint, o.NativeViewDraft = &plan, damengnative.Fingerprint(plan), &draft
	o.NativeViewEditable = false
	o.Units, o.Status, o.RequiresReview = units, "draft", !reviewed
	o.Verification, o.ManualVerification = nil, nil
	if !o.TrustedNative() {
		return o, fmt.Errorf("视图修订缺少完整建视图和编译步骤")
	}
	return o, nil
}
