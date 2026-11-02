package datamigrate

import (
	"context"
	"fmt"
	"sync"

	"dbgold/datamigrate/repair"
)

// Table identities always refer to original source names. Target name folding
// must not merge two distinct source tables into the same failure record.
type migrationTableFailure struct {
	stage string
	err   error
}

type migrationTableState struct {
	mu       sync.RWMutex
	failures map[string]migrationTableFailure
	objects  map[ObjectRef]error
}

func (m *Migrator) blockMigrationObject(ref ObjectRef, err error) {
	if err == nil {
		return
	}
	m.tableState.mu.Lock()
	defer m.tableState.mu.Unlock()
	if m.tableState.objects == nil {
		m.tableState.objects = map[ObjectRef]error{}
	}
	m.tableState.objects[ref] = err
}

func (m *Migrator) migrationObjectError(ref ObjectRef) error {
	m.tableState.mu.RLock()
	defer m.tableState.mu.RUnlock()
	return m.tableState.objects[ref]
}

func (m *Migrator) blockMigrationTable(table, stage string, err error) {
	if table == "" || err == nil {
		return
	}
	m.tableState.mu.Lock()
	defer m.tableState.mu.Unlock()
	if m.tableState.failures == nil {
		m.tableState.failures = map[string]migrationTableFailure{}
	}
	if _, exists := m.tableState.failures[table]; !exists {
		m.tableState.failures[table] = migrationTableFailure{stage: stage, err: err}
	}
}

func (m *Migrator) migrationTableError(table string) error {
	m.tableState.mu.RLock()
	defer m.tableState.mu.RUnlock()
	return m.tableState.failures[table].err
}

func (m *Migrator) migrationTableBlocked(table string) bool {
	return m.migrationTableError(table) != nil
}

// Some dedicated planners already hold target names. Only use a target lookup
// when every matching source identity is known, never trim or case-fold sources.
func (m *Migrator) migrationTargetTableError(target string) error {
	m.tableState.mu.RLock()
	for table, failure := range m.tableState.failures {
		if m.objName(table) == target {
			m.tableState.mu.RUnlock()
			return failure.err
		}
	}
	m.tableState.mu.RUnlock()
	// Partition preflight owns its own category accounting and removes blocked
	// roots from the ordinary table list. They still remain failed dependencies.
	if s := m.partitionState; s != nil {
		for sourceName, table := range s.tables {
			if s.blocked[sourceName] && (m.objName(sourceName) == target || table.TargetTable == target) {
				return fmt.Errorf("分区表 %s 未通过预检: %s", sourceName, table.Error)
			}
		}
	}
	return nil
}

func (m *Migrator) stopMigrationTask(ctx context.Context, err error) bool {
	if !isMigrationTaskError(err) {
		return false
	}
	m.cancelOracleDamengTask(ctx, err)
	return true
}

type migrationTaskFailure struct{ err error }

func (e migrationTaskFailure) Error() string                 { return e.err.Error() }
func (e migrationTaskFailure) Unwrap() error                 { return e.err }
func (e migrationTaskFailure) MigrationFailureScope() string { return "task" }

func (m *Migrator) recordBlockedObject(category *CategoryReport, name, table, reason string) {
	category.Failed++
	category.Items = append(category.Items, ObjectResult{
		Name: name, Table: table, SourceName: name, TargetName: m.objName(name),
		TargetTable: m.objName(table), TargetSchema: m.cfg.TargetSchema,
		Stage: "dependency", Error: fmt.Sprintf("%s；未执行关联对象", reason),
	})
}

func (m *Migrator) reconcileMigrationFailures(objects []repair.Object) {
	m.tableState.mu.RLock()
	defer m.tableState.mu.RUnlock()
	failedTarget := map[string]migrationTableFailure{}
	for table, failure := range m.tableState.failures {
		failedTarget[m.objName(table)] = failure
	}
	for i := range objects {
		o := &objects[i]
		failure, failed := m.tableState.failures[o.SourceTable]
		if !failed && (o.Kind == "table" || o.Kind == "data") {
			failure, failed = m.tableState.failures[o.SourceName]
		}
		if !failed {
			failure, failed = failedTarget[o.Table]
		}
		if !failed && o.ForeignKey != nil {
			failure, failed = failedTarget[o.ForeignKey.RefTable]
		}
		if o.Kind == "table" {
			if !failed {
				continue
			}
			if failure.stage == "data" || failure.stage == "validation" {
				if o.DataStatus != "not_required" {
					o.DataStatus = "failed"
				}
			} else {
				if o.Status == "pending" {
					o.Status, o.Error = "failed", failure.err.Error()
				}
				if o.DataStatus != "not_required" {
					o.DataStatus = "blocked"
				}
			}
			continue
		}
		if err := m.tableState.objects[ObjectRef{Kind: o.Kind, Table: o.SourceTable, Name: o.SourceName}]; err != nil {
			failure, failed = migrationTableFailure{err: err}, true
		}
		if failed && (o.Status == "pending" || o.Status == "blocked") {
			o.Status, o.Error = "blocked", "依赖对象迁移失败: "+failure.err.Error()
			for j := range o.Units {
				if o.Units[j].Status == "pending" {
					o.Units[j].Status, o.Units[j].Error = "blocked", o.Error
				}
			}
		}
	}
	// Resolve dependency chains to a fixed point, including views depending on
	// other blocked views, without changing any executed/unknown SQL outcome.
	for {
		states := map[string]string{}
		for _, o := range objects {
			states[o.ID] = o.Status
		}
		changed := false
		for i := range objects {
			o := &objects[i]
			if o.Status != "pending" {
				continue
			}
			for _, dep := range o.Dependencies {
				if status := states[dep]; status != "" && status != "succeeded" {
					o.Status, o.Error = "blocked", "依赖对象未成功迁移"
					for j := range o.Units {
						if o.Units[j].Status == "pending" {
							o.Units[j].Status, o.Units[j].Error = "blocked", o.Error
						}
					}
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
}
