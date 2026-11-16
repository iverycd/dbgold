package datamigrate

import (
	"context"
	"fmt"
	"slices"

	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
)

// Ownership comes only from the persisted execution ledger. Matching names or
// a trigger marker by themselves never authorize reuse of an existing sequence.
func (m *Migrator) bindPGOracleSequenceEvidence(ctx context.Context, p *PGOraclePreflight) {
	evidence := PGOracleSequenceEvidence(ctx)
	for ref, plan := range p.sequences {
		var saved *pgoracle.SequencePlan
		var problem error
		for i := range evidence {
			candidate := &evidence[i]
			if candidate.Schema != plan.Schema || candidate.Name != plan.Name || candidate.Source.Schema != plan.Source.Schema || candidate.Source.Name != plan.Source.Name || candidate.Identity == nil {
				continue
			}
			if err := comparePGOracleRecoverySequence(candidate.Source, plan.Source); err != nil {
				problem = err
				break
			}
			if saved != nil && (*saved.Identity != *candidate.Identity || repair.Hash(saved.Bindings) != repair.Hash(candidate.Bindings)) {
				problem = fmt.Errorf("序列 %s 的持久化身份或完整绑定冲突", plan.Name)
				break
			}
			for _, binding := range plan.Bindings {
				if !slices.Contains(candidate.Bindings, binding) {
					problem = fmt.Errorf("序列 %s 的列映射偏离原工具计划", plan.Name)
					break
				}
			}
			saved = candidate
		}
		if problem == nil && saved != nil {
			plan.Identity = saved.Identity
			// Shared consumers outside the current selection must remain frozen
			// and take part in drift checks and MAX/MIN calibration.
			plan.Bindings = slices.Clone(saved.Bindings)
			if inspector, ok := m.writer.(pgOracleRecoveryWriter); ok {
				problem = inspector.InspectPGOracleSequence(ctx, *plan)
				if problem == nil {
					problem = inspector.ValidatePGOracleSequenceBindings(ctx, *plan, false)
				}
			} else {
				problem = fmt.Errorf("目标缺少工具序列归属与绑定核验能力")
			}
		}
		if problem != nil {
			p.add(ref, problem)
			if p.dataOnly {
				for _, b := range plan.Bindings {
					p.add(tableRef(b.SourceTable), fmt.Errorf("数据装载前序列核验失败: %w", problem))
				}
			}
			m.stopMigrationTask(ctx, problem)
		}
		if p.dataOnly && plan.Identity == nil {
			p.warn(ref, "仅数据迁移没有此序列的工具创建记录，不自动校准；目标业务触发器仍须通过只读检查")
			delete(p.sequences, ref)
			continue
		}
		if p.dataOnly && plan.Identity != nil {
			statements, err := plan.SequenceStatements()
			p.add(ref, err)
			p.Objects = append(p.Objects, PlannedObject{Object: ref, Target: plan.Name, Stage: "sequence", Statements: statements})
			// A failed page may have preceding committed rows. Calibration must
			// still account for them; the binding/identity checks above protect
			// every existing consumer independently of a table's data status.
			p.dependencies[ref] = nil
		} else if plan.Identity != nil {
			for i := range p.Objects {
				if p.Objects[i].Object == ref {
					statements, err := plan.SequenceStatements()
					p.Objects[i].Statements = statements
					p.add(ref, err)
				}
			}
		}
	}
}
