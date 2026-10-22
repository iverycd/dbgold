package datamigrate

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/validation"
)

// Validate's normal schema-change checks remain strict. This adapter proves the
// entire original metadata on every Describe before projecting the explicitly
// rebased physical outputs out of the ordinary-value comparison.
type nativeRowIDProjectionScanner struct {
	validation.Scanner
	expected validation.Table
	ignored  map[string]bool
}

func (s *nativeRowIDProjectionScanner) DescribeValidationTable(ctx context.Context, name string) (validation.Table, error) {
	actual, err := s.Scanner.DescribeValidationTable(ctx, name)
	if err != nil {
		return actual, err
	}
	if len(actual.Columns) != len(s.expected.Columns) || !slices.Equal(actual.PrimaryKey, s.expected.PrimaryKey) {
		return validation.Table{}, fmt.Errorf("物化快照完整列数或主键偏离冻结定义")
	}
	byName := map[string]validation.Column{}
	for _, c := range actual.Columns {
		if _, exists := byName[c.Name]; exists {
			return validation.Table{}, fmt.Errorf("物化快照列名重复: %s", c.Name)
		}
		byName[c.Name] = c
	}
	for _, c := range s.expected.Columns {
		if !reflect.DeepEqual(byName[c.Name], c) {
			return validation.Table{}, fmt.Errorf("物化快照列 %s 偏离完整冻结定义", c.Name)
		}
	}
	var projected []validation.Column
	for _, c := range actual.Columns {
		if !s.ignored[c.Name] {
			projected = append(projected, c)
		}
	}
	actual.Columns = projected
	for _, key := range actual.PrimaryKey {
		if s.ignored[key] {
			actual.PrimaryKey = nil
			break
		}
	}
	return actual, nil
}

// Keep full-content validation on every ordinary column. Direct ROWIDs have
// already been covered by source full-bag and target before/after baseline
// proofs; byte equality to physical addresses in another database is false.
func (m *Migrator) markNativeRowIDValidation(p *damengnative.Preflight, states map[string]error) {
	if !m.dataValidationEnabled() || m.repairs == nil {
		return
	}
	historyVerified := map[string]bool{}
	m.repairs.mu.Lock()
	for _, object := range m.repairs.objects {
		if object.Native == nil || object.Native.Materialized == nil || object.Native.Materialized.History == nil {
			continue
		}
		for i, unit := range object.Units {
			if i < len(object.Native.Units) && object.Native.Units[i].Phase == "mv_history" && unit.Status == "succeeded" {
				for _, view := range object.Native.Materialized.History.Views {
					historyVerified[view.ObjectID] = true
				}
			}
		}
	}
	m.repairs.mu.Unlock()
	for _, o := range p.Objects {
		state := o.Materialized
		if state == nil || !state.Baseline || len(state.RowIDColumns) == 0 || states[o.ID] != nil {
			continue
		}
		baseline := damengnative.MaterializedBaselineSQL(o.Schema, o.Name)
		verified := historyVerified[o.ID]
		m.repairs.mu.Lock()
		for _, object := range m.repairs.objects {
			if object.ID != o.ID {
				continue
			}
			for _, unit := range object.Units {
				if unit.SQL == baseline && unit.Status == "succeeded" {
					verified = true
				}
			}
		}
		m.repairs.mu.Unlock()
		if !verified {
			continue
		}
		for i := range m.validationTables {
			frozen := &m.validationTables[i]
			if frozen.target != o.Table {
				continue
			}
			if frozen.rebasedRowIDs == nil {
				frozen.rebasedRowIDs = map[string]bool{}
			}
			for _, ordinal := range state.RowIDColumns {
				if ordinal >= 0 && ordinal < len(state.SnapshotColumns) {
					frozen.rebasedRowIDs[state.SnapshotColumns[ordinal]] = true
				}
			}
			frozen.note = "直接 ROWID 已通过源端全列、目标基线前普通列及基线后全列多重集核验，映射为目标物理地址；本次内容校验比较全部普通列与重复行数量，不比较源目标物理 ROWID 字节。"
			if historyVerified[o.ID] {
				frozen.note = "陈旧快照及待处理日志的直接 ROWID 已通过同一物理地址映射及完整多重集核验，包含源端已删除行；本次内容校验比较全部普通列与重复行数量，不比较源目标物理 ROWID 字节。"
			}
		}
	}
}
