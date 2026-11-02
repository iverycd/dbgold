package datamigrate

import (
	"errors"
	"fmt"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

// nativeScopedFailure preserves the original cause and distinguishes errors
// that ran SQL from objects withheld because their prerequisites were unusable.
type nativeScopedFailure struct {
	stage string
	cause error
}

func (e *nativeScopedFailure) Error() string { return e.cause.Error() }
func (e *nativeScopedFailure) Unwrap() error { return e.cause }
func nativeObjectFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &nativeScopedFailure{stage: stage, cause: err}
}
func nativeFailureStage(err error) string {
	var scoped *nativeScopedFailure
	if errors.As(err, &scoped) {
		return scoped.stage
	}
	return "execute"
}

func propagateNativeFailures(objects []damengnative.Object, states map[string]error) {
	for changed := true; changed; {
		changed = false
		for _, o := range objects {
			if states[o.ID] != nil {
				continue
			}
			deps := append([]string{}, o.Dependencies...)
			if o.ParentID != "" {
				deps = append(deps, o.ParentID)
			}
			for _, dep := range deps {
				if err := states[dep]; err != nil {
					states[o.ID] = nativeObjectFailure("dependency", fmt.Errorf("依赖对象 %s 未完成: %w", dep, err))
					changed = true
					break
				}
			}
		}
	}
}

func nativePreflightStates(p *damengnative.Preflight) map[string]error {
	states := map[string]error{}
	for id, issue := range p.Blocked {
		stage := p.BlockedStages[id]
		if stage == "" {
			stage = "preflight"
		}
		states[id] = nativeObjectFailure(stage, errors.New(issue.Reason))
	}
	propagateNativeFailures(p.Objects, states)
	return states
}

func finalizeNativePreflight(p *damengnative.Preflight) {
	oldStages := p.BlockedStages
	p.Blocked = map[string]damengnative.Issue{}
	p.BlockedStages = map[string]string{}
	objects := map[string]damengnative.Object{}
	for _, o := range p.Objects {
		objects[o.ID] = o
	}
	declared := damengnative.DeclaredTableChildren(p.Objects)
	for _, o := range declared {
		objects[o.ID] = o
	}
	byTargetID := map[string]damengnative.Object{}
	for _, o := range p.TargetObjects {
		byTargetID[o.ID] = o
	}
	global := false
	for _, issue := range p.Issues {
		if issue.Severity == "warning" {
			continue
		}
		o, exists := objects[issue.ObjectID]
		if !exists {
			if target, known := byTargetID[issue.ObjectID]; known {
				for _, candidate := range p.Objects {
					if nativeCollisionKey(candidate) == nativeCollisionKey(target) {
						o, exists = candidate, true
						break
					}
				}
			}
		}
		if issue.ObjectID == "" || !exists {
			global = true
			continue
		}
		// Inline objects are created by their owning statement: withholding only
		// their inventory entry would still execute the unsafe parent CREATE.
		seen := map[string]bool{}
		for o.Managed && !seen[o.ID] {
			seen[o.ID] = true
			parent, ok := objects[o.ParentID]
			if !ok {
				for _, candidate := range p.Objects {
					if candidate.Kind == "table" && candidate.Name == o.Table {
						parent, ok = candidate, true
						break
					}
				}
			}
			if !ok {
				global = true
				break
			}
			o = parent
		}
		issue.ObjectID = o.ID
		p.Blocked[o.ID] = issue
		p.BlockedStages[o.ID] = oldStages[o.ID]
		if p.BlockedStages[o.ID] == "" {
			p.BlockedStages[o.ID] = "preflight"
		}
	}
	states := nativePreflightStates(p)
	protectNativeRetainedTargets(p, states)
	propagateNativeFailures(p.Objects, states)
	for id, err := range states {
		if _, exists := p.Blocked[id]; exists {
			continue
		}
		o := objects[id]
		issue := damengnative.Issue{ObjectID: id, Kind: o.Kind, Name: o.Name, Severity: "error", Reason: err.Error()}
		p.Blocked[id] = issue
		p.BlockedStages[id] = nativeFailureStage(err)
		p.Issues = append(p.Issues, issue)
	}
	p.Ready = !damengnative.HasErrors(p.Issues)
	p.CanStart = p.Ready && !global
	for i := range p.Objects {
		if states[p.Objects[i].ID] != nil {
			p.Objects[i].Status = "blocked"
		} else if !global {
			p.CanStart = true
		}
	}
	// A withheld replacement must not remove the existing target object.
	drops := p.Drops[:0]
	for _, drop := range p.Drops {
		if !nativeDropBlocked(p, drop, states) {
			drops = append(drops, drop)
		}
	}
	p.Drops = drops
}

func nativeDropBlocked(p *damengnative.Preflight, drop damengnative.Object, states map[string]error) bool {
	for _, o := range p.Objects {
		if nativeCollisionKey(o) == nativeCollisionKey(drop) {
			return states[o.ID] != nil
		}
	}
	return true
}

func nativeBlockDrop(p *damengnative.Preflight, drop damengnative.Object, states map[string]error, err error) {
	for _, o := range p.Objects {
		if nativeCollisionKey(o) == nativeCollisionKey(drop) {
			states[o.ID] = nativeObjectFailure("execute", fmt.Errorf("删除既有目标对象 %s 失败: %w", drop.Name, err))
		}
	}
	protectNativeRetainedTargets(p, states)
	propagateNativeFailures(p.Objects, states)
}

// A retained target child may still constrain its parent. Follow the frozen
// target graph before allowing further DROP statements, including failed drops.
func protectNativeRetainedTargets(p *damengnative.Preflight, states map[string]error) {
	sourceByName := map[string]damengnative.Object{}
	targetByID := map[string]damengnative.Object{}
	for _, o := range p.Objects {
		sourceByName[nativeCollisionKey(o)] = o
	}
	for _, o := range p.TargetObjects {
		targetByID[o.ID] = o
	}
	for changed := true; changed; {
		changed = false
		for _, target := range p.TargetObjects {
			// Engine-managed and table-owned children disappear with a healthy
			// replacement of their owner; they are not retained dependants.
			if target.Managed || nativeOwnedObjectKind(target) {
				owner := target.ParentID
				if owner == "" {
					for _, candidate := range p.TargetObjects {
						if candidate.Kind == "table" && candidate.Name == target.Table {
							owner = candidate.ID
							break
						}
					}
				}
				seen := map[string]bool{}
				removedWithOwner := false
				for owner != "" && !seen[owner] {
					seen[owner] = true
					parent, exists := targetByID[owner]
					if !exists {
						break
					}
					if replacement, exists := sourceByName[nativeCollisionKey(parent)]; exists && states[replacement.ID] == nil {
						removedWithOwner = true
						break
					}
					owner = parent.ParentID
				}
				if removedWithOwner {
					continue
				}
			}
			source, replacing := sourceByName[nativeCollisionKey(target)]
			if replacing && states[source.ID] == nil {
				continue
			}
			for _, dependency := range target.Dependencies {
				parent, exists := targetByID[dependency]
				if !exists {
					continue
				}
				replacement, exists := sourceByName[nativeCollisionKey(parent)]
				if exists && states[replacement.ID] == nil {
					states[replacement.ID] = nativeObjectFailure("dependency", fmt.Errorf("保留目标对象 %s 仍依赖 %s，未删除关联对象", target.Name, parent.Name))
					changed = true
				}
			}
		}
		propagateNativeFailures(p.Objects, states)
	}
}

func (m *Migrator) nativePersistenceFailed() bool {
	if m.repairs == nil {
		return false
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	return m.repairs.sinkErr != nil
}

func (m *Migrator) syncNativeBlockedTables(p *damengnative.Preflight, states map[string]error) {
	propagateNativeFailures(p.Objects, states)
	for _, table := range p.Snapshot.Tables {
		if err := states[table.ObjectID]; err != nil {
			m.blockMigrationTable(nativeSourceTable(table), nativeFailureStage(err), err)
		}
	}
}

func nativeHistoryMembers(p *damengnative.Preflight, leader damengnative.Object) map[string]bool {
	members := map[string]bool{}
	byID := map[string]damengnative.Object{}
	byName := map[string]string{}
	for _, o := range p.Objects {
		byID[o.ID] = o
		if o.Kind == "table" {
			byName[o.Name] = o.ID
		}
	}
	var visit func(string)
	visit = func(id string) {
		if id == "" || members[id] {
			return
		}
		members[id] = true
		o := byID[id]
		for _, dep := range o.Dependencies {
			visit(dep)
		}
		visit(o.ParentID)
	}
	visit(leader.ID)
	if leader.Materialized == nil || leader.Materialized.History == nil {
		return members
	}
	history := leader.Materialized.History
	for _, view := range history.Views {
		visit(view.ObjectID)
		visit(byName[view.Table])
		visit(byName[view.Staging])
		visit(byName[view.Buffer.Name])
		for _, table := range view.RowIDTables {
			visit(byName[table])
		}
		if view.EmptyTouch != nil {
			visit(byName[view.EmptyTouch.Table])
		}
		for _, buffer := range view.Buffers {
			visit(byName[buffer.Name])
		}
	}
	for _, log := range history.Logs {
		visit(log.ObjectID)
		visit(byName[log.BaseTable])
		visit(byName[log.Table])
		visit(byName[log.Staging])
		visit(byName[log.Buffer.Name])
	}
	for _, mapping := range history.RowIDMaps {
		visit(byName[mapping.BaseTable])
		visit(byName[mapping.Table])
	}
	return members
}

func blockNativeHistoryGroups(p *damengnative.Preflight, states map[string]error) {
	for _, leader := range p.Objects {
		if leader.Materialized == nil || leader.Materialized.History == nil {
			continue
		}
		members := nativeHistoryMembers(p, leader)
		var cause error
		for id := range members {
			if states[id] != nil {
				cause = states[id]
				break
			}
		}
		if cause == nil {
			continue
		}
		for id := range members {
			o := damengnative.Object{}
			for _, candidate := range p.Objects {
				if candidate.ID == id {
					o = candidate
					break
				}
			}
			if o.Materialized != nil && states[id] == nil {
				states[id] = nativeObjectFailure("dependency", fmt.Errorf("冻结物化历史组未完成，未恢复历史或自动刷新: %w", cause))
			}
		}
	}
	propagateNativeFailures(p.Objects, states)
}

func nativeObjectClosure(objects []damengnative.Object, id string) map[string]bool {
	return nativeHistoryMembers(&damengnative.Preflight{Objects: objects}, damengnative.Object{ID: id})
}

func (m *Migrator) nativeScopeReady(objects []damengnative.Object, members map[string]bool, states map[string]error) bool {
	for id := range members {
		if states[id] != nil {
			return false
		}
	}
	if m.repairs == nil {
		return false
	}
	m.repairs.mu.Lock()
	defer m.repairs.mu.Unlock()
	if m.repairs.sinkErr != nil {
		return false
	}
	for _, object := range m.repairs.objects {
		if !members[object.ID] || object.DiagnosticOnly {
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

func markNativeBlockedRecord(o *repair.Object, err error) {
	if err == nil {
		return
	}
	stage := nativeFailureStage(err)
	if stage != "dependency" && stage != "preflight" {
		return
	}
	o.Status, o.Error = "blocked", err.Error()
	for i := range o.Units {
		if o.Units[i].Status == "pending" {
			o.Units[i].Status, o.Units[i].Error = "blocked", err.Error()
		}
	}
	if o.DataStatus == "pending" {
		o.DataStatus = "blocked"
	}
}

func nativePolicyMembers(p *damengnative.Preflight, object damengnative.Object) map[string]bool {
	for _, leader := range p.Objects {
		if leader.Materialized == nil || leader.Materialized.History == nil {
			continue
		}
		members := nativeHistoryMembers(p, leader)
		if members[object.ID] {
			return members
		}
	}
	return nativeObjectClosure(p.Objects, object.ID)
}

func nativePolicyValidated(report *MigrationReport, p *damengnative.Preflight, members map[string]bool) bool {
	for _, issue := range report.NativeSchema.Verification {
		if issue.Severity != "warning" && (issue.ObjectID == "" || members[issue.ObjectID]) {
			return false
		}
	}
	if report.DataValidation == nil {
		return true
	}
	hasDataScope := false
	for _, table := range p.Snapshot.Tables {
		if !members[table.ObjectID] || table.Shadow {
			continue
		}
		hasDataScope = true
		passed := false
		for _, result := range report.DataValidation.Tables {
			if result.SourceTable == nativeSourceTable(table) && result.Status == "passed" {
				passed = true
				break
			}
		}
		if !passed {
			return false
		}
	}
	return hasDataScope || !report.DataValidation.HasFailures()
}
