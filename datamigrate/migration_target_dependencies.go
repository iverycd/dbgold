package datamigrate

import (
	"context"
	"dbgold/datamigrate/target"
	"fmt"
	"strings"
)

type migrationDependencyInspector interface {
	InspectMigrationDependencies(context.Context) (target.MigrationDependencies, error)
}

// protectRetainedMigrationTables protects physical target children retained by
// local preflight failures. Normal tasks do not acquire additional catalog reads.
func (m *Migrator) protectRetainedMigrationTables(ctx context.Context, tables []string, report *MigrationReport, completed ...map[string]bool) bool {
	if m.cfg.Content == "data_only" || m.cfg.objectMode() {
		return true
	}
	retained := map[string]bool{}
	targetNames := map[string]string{}
	scope := append([]string{}, tables...)
	for _, table := range tables {
		targetNames[table] = m.objName(table)
		retained[table] = m.migrationTableBlocked(table)
	}
	if state := m.partitionState; state != nil {
		state.mu.Lock()
		for table, blocked := range state.blocked {
			if !blocked {
				continue
			}
			if _, exists := targetNames[table]; !exists {
				scope = append(scope, table)
			}
			retained[table] = true
			targetNames[table] = m.objName(table)
			if result := state.tables[table]; result != nil && result.TargetTable != "" {
				targetNames[table] = result.TargetTable
			}
		}
		state.mu.Unlock()
	}
	blocked, healthy := false, false
	for _, table := range scope {
		if retained[table] {
			blocked = true
		} else if len(completed) == 0 || !completed[0][table] {
			healthy = true
		}
	}
	if !blocked {
		return true
	}
	if !healthy {
		// No remaining DROP can invalidate a retained reference. Continue to
		// data/post-DDL for completed tables and independent objects.
		return true
	}
	fail := func(err error) bool {
		err = fmt.Errorf("检查保留目标表的外键依赖失败，未继续重建: %w", err)
		m.stopMigrationTask(ctx, migrationTaskFailure{err: err})
		if report != nil {
			report.Tables.Failed++
			report.Tables.Total = max(report.Tables.Total, report.Tables.Failed+report.Tables.Success)
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: "目标依赖检查", Stage: "task", Error: err.Error()})
		}
		m.log.Errorf("%v", err)
		return false
	}
	inspector, ok := m.writer.(migrationDependencyInspector)
	if !ok {
		return fail(fmt.Errorf("目标 Writer 未提供完整目标依赖检查能力"))
	}
	snapshot, err := inspector.InspectMigrationDependencies(ctx)
	if err != nil {
		return fail(err)
	}
	if snapshot.Schema == "" {
		return fail(fmt.Errorf("目标实际 schema 为空"))
	}
	key := func(name string) string {
		if snapshot.FoldNames {
			return strings.ToLower(name)
		}
		return name
	}
	for _, dep := range snapshot.Relations {
		if dep.Schema == "" || dep.Table == "" || dep.Name == "" || dep.RefSchema == "" || dep.RefTable == "" {
			return fail(fmt.Errorf("目标依赖目录包含不完整的对象身份"))
		}
	}
	names := map[string][]string{}
	for _, table := range scope {
		name := key(targetNames[table])
		names[name] = append(names[name], table)
	}
	for changed := true; changed; {
		changed = false
		for _, dep := range snapshot.Relations {
			if key(dep.RefSchema) != key(snapshot.Schema) {
				continue
			}
			parents := names[key(dep.RefTable)]
			if len(parents) == 0 {
				continue
			}
			childRebuilt := key(dep.Schema) == key(snapshot.Schema) && len(names[key(dep.Table)]) > 0
			if childRebuilt {
				for _, child := range names[key(dep.Table)] {
					if retained[child] || m.migrationTableBlocked(child) {
						childRebuilt = false
						break
					}
				}
			}
			if childRebuilt {
				continue
			}
			for _, parent := range parents {
				if len(completed) > 0 && completed[0][parent] {
					continue
				}
				if retained[parent] || m.migrationTableBlocked(parent) {
					continue
				}
				err := fmt.Errorf("目标保留表 %s.%s 的外键 %s 引用该表，跳过重建以保留旧目标依赖", dep.Schema, dep.Table, dep.Name)
				m.blockMigrationTable(parent, "dependency", err)
				if report != nil {
					report.Tables.Failed++
					report.Tables.Total = max(report.Tables.Total, report.Tables.Failed+report.Tables.Success)
					report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: parent, Table: parent, Stage: "dependency", Error: err.Error()})
				}
				m.log.Warnf("跳过表 %s: %v", parent, err)
				changed = true
			}
		}
	}
	return true
}
