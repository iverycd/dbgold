package datamigrate

import (
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Diagnostic identity and execution ownership are deliberately separate. A
// non-excludable table error is still local; unknown shared metadata is global.
func (p *PGMySQLPreflight) blockIssue(ref ObjectRef, err error) {
	if p.blocked == nil {
		p.blocked = map[ObjectRef]error{}
	}
	switch {
	case ref.Kind == "table", ref.Kind == "view":
		p.blocked[ref] = err
	case ref.Table != "":
		p.blocked[tableRef(ref.Table)] = err
	case ref.Kind == "metadata", ref.Kind == "target", ref.Kind == "permission", ref.Kind == "config":
		p.globalError = true
	default:
		p.blocked[ref] = err
	}
}

func (p *PGMySQLPreflight) objectBlocked(ref ObjectRef) bool {
	return p.blockedDependency(ref, map[ObjectRef]bool{}) != nil
}
func (p *PGMySQLPreflight) blockedDependency(ref ObjectRef, seen map[ObjectRef]bool) error {
	if err := p.blocked[ref]; err != nil {
		return err
	}
	if seen[ref] {
		return nil
	}
	seen[ref] = true
	if ref.Table != "" {
		if err := p.blocked[tableRef(ref.Table)]; err != nil {
			return err
		}
	}
	for _, dep := range p.dependencies[ref] {
		if err := p.blockedDependency(dep, seen); err != nil {
			return err
		}
	}
	return nil
}
func (p *PGMySQLPreflight) linkTableDependencies() {
	if p.dependencies == nil {
		p.dependencies = map[ObjectRef][]ObjectRef{}
	}
	for _, obj := range p.Objects {
		if obj.Object.Table != "" && obj.Stage != "drop_dependency" {
			p.dependencies[obj.Object] = append(p.dependencies[obj.Object], tableRef(obj.Object.Table))
		}
	}
	for _, t := range p.tables {
		for _, fk := range t.ForeignKeys {
			ref := ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}
			p.dependencies[ref] = append(p.dependencies[ref], tableRef(fk.RefTable))
		}
	}
}

func validateMySQLObjectName(name string, relation bool) error {
	for _, r := range name {
		if r > 0xffff {
			return fmt.Errorf("MySQL 标识符不支持补充平面字符")
		}
	}
	if strings.HasSuffix(name, " ") || relation && strings.ContainsAny(name, "./\\") {
		return fmt.Errorf("目标对象名含 MySQL 不允许的字符或尾空格")
	}
	if name == "" || strings.ContainsRune(name, 0) || utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("目标标识符为空、含 NUL 或超过 64 字符")
	}
	return nil
}

// Keep dependency skips distinct from executable failures and pending work.
func (m *Migrator) recordPGBlockedObject(obj PlannedObject, err error, dependencies ...ObjectRef) {
	if m.repairs == nil {
		return
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	table := m.objName(obj.Object.Table)
	id := repairID(obj.Object.Kind, table, m.objName(obj.Object.Name))
	idx := r.objectPosition(id)
	if idx < 0 {
		idx = r.appendObject(repair.Object{ID: id, Kind: obj.Object.Kind, Name: obj.Object.Name, SourceName: obj.Object.Name, SourceTable: obj.Object.Table, Table: table, TargetName: m.objName(obj.Object.Name), Schema: m.cfg.TargetSchema, DDL: dialect.JoinSQL(obj.Statements), Units: unitsFor(obj.Statements), Dependencies: []string{}})
	}
	o := &r.objects[idx]
	o.Status = "blocked"
	o.Error = err.Error()
	o.RequiresReview = true
	for i := range o.Units {
		o.Units[i].Status = "blocked"
		o.Units[i].Error = err.Error()
	}
	dependencies = append(dependencies, ObjectRef{Kind: "table", Name: obj.Object.Table})
	for _, dep := range dependencies {
		if dep.Name == "" {
			continue
		}
		table := ""
		if dep.Kind == "table" {
			table = m.objName(dep.Name)
		}
		id := repairID(dep.Kind, table, m.objName(dep.Name))
		if !slices.Contains(o.Dependencies, id) {
			o.Dependencies = append(o.Dependencies, id)
		}
	}
	_ = m.flushRepairObjectLocked(*o)
}

func (p *PGMySQLPreflight) protectTargetDependencies(m *Migrator) {
	names := map[string]string{}
	key := func(s string) string { return oracleMySQLTargetKey(s, p.Target.LowerCaseTableNames) }
	for _, t := range p.tables {
		names[key(m.objName(t.Info.TableName))] = t.Info.TableName
	}
	for changed := true; changed; {
		changed = false
		for _, fk := range p.targetSnapshot {
			parent, ok := names[key(fk.RefTable)]
			if !ok || key(fk.RefSchema) != key(p.Target.Database) || p.objectBlocked(tableRef(parent)) {
				continue
			}
			child, ok := names[key(fk.Table)]
			if !ok || key(fk.Schema) != key(p.Target.Database) || p.objectBlocked(tableRef(child)) {
				p.add(tableRef(parent), fmt.Sprintf("目标表被保留的 %s.%s 外键 %s 引用，未执行 DROP", fk.Schema, fk.Table, fk.ConstraintName), false)
				changed = true
			}
		}
	}
	kept := p.targetDependencies[:0]
	for _, fk := range p.targetDependencies {
		if !p.objectBlocked(tableRef(names[key(fk.Table)])) && !p.objectBlocked(tableRef(names[key(fk.RefTable)])) {
			kept = append(kept, fk)
		}
	}
	p.targetDependencies = kept
}
