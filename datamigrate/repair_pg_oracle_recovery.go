package datamigrate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

type pgOracleRecoveryWriter interface {
	ConfigurePGOracle(map[string][]source.ColumnInfo)
	ValidatePGOracleDataTable(context.Context, string, []source.ColumnInfo, map[string]string) error
	InspectPGOracleSequence(context.Context, pgoracle.SequencePlan) error
	ValidatePGOracleSequenceBindings(context.Context, pgoracle.SequencePlan, bool) error
}

type pgOracleRecoveryEvidenceKey struct{}

// WithPGOracleRecoveryEvidence supplies persisted sequence ownership without
// selecting those successful objects for another execution.
func WithPGOracleRecoveryEvidence(ctx context.Context, plans []pgoracle.SequencePlan) context.Context {
	return context.WithValue(ctx, pgOracleRecoveryEvidenceKey{}, clonePGOracleSequenceEvidence(plans))
}

func PGOracleSequenceEvidence(ctx context.Context) []pgoracle.SequencePlan {
	plans, _ := ctx.Value(pgOracleRecoveryEvidenceKey{}).([]pgoracle.SequencePlan)
	return clonePGOracleSequenceEvidence(plans)
}

func clonePGOracleSequenceEvidence(plans []pgoracle.SequencePlan) []pgoracle.SequencePlan {
	if plans == nil {
		return nil
	}
	// The durable plan contains only JSON-compatible metadata. Deep copies keep
	// later repair identity updates from changing another preflight's evidence.
	data, err := json.Marshal(plans)
	if err != nil {
		return nil
	}
	var copy []pgoracle.SequencePlan
	if err := json.Unmarshal(data, &copy); err != nil {
		return nil
	}
	return copy
}

func preparePGOracleRecovery(ctx context.Context, r source.Reader, w target.Writer, p *repair.RecoveryPreview, related []repair.Object) error {
	reader, ok := r.(interface {
		ReadPGOracleSnapshot(context.Context, []string) (*source.PGSnapshot, error)
	})
	if !ok {
		return fmt.Errorf("PG 源库缺少补迁原始快照能力")
	}
	writer, ok := w.(pgOracleRecoveryWriter)
	if !ok {
		return fmt.Errorf("Oracle Writer 缺少 PG 补迁能力")
	}
	snapshot, err := reader.ReadPGOracleSnapshot(ctx, []string{p.SourceTable})
	if err != nil {
		return err
	}
	if snapshot == nil {
		return fmt.Errorf("PG 补迁快照为空")
	}
	if len(snapshot.Tables) != 1 || snapshot.Tables[0].Info.TableName != p.SourceTable {
		return fmt.Errorf("补迁源表身份已变化")
	}
	for _, issue := range snapshot.Tables[0].Unsupported {
		if issue.Kind == "table" || issue.Kind == "metadata" {
			return fmt.Errorf("源表 %s 元数据不可用于补迁: %s", p.SourceTable, issue.Reason)
		}
	}
	if repair.Hash(snapshot.Tables[0].Info.Columns) != repair.Hash(p.SourceColumns) {
		return fmt.Errorf("源表结构在补迁预检期间变化")
	}
	saved := map[string]pgoracle.SequencePlan{}
	candidates := PGOracleSequenceEvidence(ctx)
	for _, o := range related {
		if o.PGOracleSequence == nil {
			continue
		}
		candidates = append(candidates, *o.PGOracleSequence)
	}
	for _, plan := range candidates {
		key := plan.Source.Schema + "\x00" + plan.Source.Name
		if old, exists := saved[key]; exists {
			a, b := old, plan
			a.Identity, b.Identity = nil, nil
			a.Source, b.Source = pgOracleSequenceDefinition(a.Source), pgOracleSequenceDefinition(b.Source)
			if repair.Hash(a) != repair.Hash(b) {
				return fmt.Errorf("序列 %s 的修复对象包含不同冻结方案", plan.Name)
			}
			if old.Identity != nil && plan.Identity != nil && *old.Identity != *plan.Identity {
				return fmt.Errorf("序列 %s 的修复身份冲突", plan.Name)
			}
			if plan.Identity == nil {
				plan.Identity = old.Identity
			}
		}
		saved[key] = plan
	}
	allowed := map[string]string{}
	needsSequences := false
	for _, c := range p.SourceColumns {
		needsSequences = needsSequences || c.Identity != "" && c.Identity != "0" || c.Default != nil && strings.Contains(strings.ToLower(*c.Default), "nextval")
	}
	for _, plan := range saved {
		for _, b := range plan.Bindings {
			needsSequences = needsSequences || b.SourceTable == p.SourceTable
		}
	}
	if needsSequences {
		for _, issue := range snapshot.Objects {
			if issue.Kind == "sequence" && issue.Reason != "" && issue.Table == "" {
				return fmt.Errorf("补迁序列元数据不可确认: %s", issue.Reason)
			}
		}
	}
	observed := map[string]bool{}
	for _, current := range snapshot.Sequences {
		bound := false
		for _, b := range current.Bindings {
			bound = bound || b.Schema == snapshot.Profile.Schema && b.TableName == p.SourceTable
		}
		if !bound {
			continue
		}
		observed[current.Schema+"\x00"+current.Name] = true
		plan, exists := saved[current.Schema+"\x00"+current.Name]
		if !exists {
			return fmt.Errorf("序列 %s 缺少冻结工具方案，请先补齐序列修复记录", current.Name)
		}
		if err := comparePGOracleRecoverySequence(plan.Source, current); err != nil {
			return err
		}
		// Preserve the initial SQL's source state in repair objects; only this
		// data-recovery preview freezes the newly observed source watermark.
		plan.Source = current
		plan.Bindings = slices.Clone(plan.Bindings)
		for _, b := range plan.Bindings {
			if b.SourceTable == p.SourceTable && (b.Table != p.TargetTable || p.ColumnMapping[b.SourceColumn] != b.Column) {
				return fmt.Errorf("序列 %s 的列映射已变化，冻结触发器不能自动改绑", plan.Name)
			}
			if b.Table == p.TargetTable && plan.Identity != nil {
				allowed[b.Trigger] = plan.TriggerSQL(b)
			}
		}
		if err := writer.InspectPGOracleSequence(ctx, plan); err != nil {
			return err
		}
		if err := writer.ValidatePGOracleSequenceBindings(ctx, plan, false); err != nil {
			return err
		}
		p.PGOracleSequences = append(p.PGOracleSequences, plan)
	}
	for key, plan := range saved {
		for _, b := range plan.Bindings {
			if b.SourceTable == p.SourceTable && !observed[key] {
				return fmt.Errorf("源序列 %s 的冻结绑定已不存在或元数据不可确认", plan.Source.Name)
			}
		}
	}
	mapped := slices.Clone(p.SourceColumns)
	for i := range mapped {
		mapped[i].Name = p.ColumnMapping[mapped[i].Name]
	}
	if err := writer.ValidatePGOracleDataTable(ctx, p.TargetTable, mapped, allowed); err != nil {
		return err
	}
	writer.ConfigurePGOracle(map[string][]source.ColumnInfo{p.TargetTable: mapped})
	return nil
}

func comparePGOracleRecoverySequence(saved, current source.PGSequence) error {
	if current.Unsupported != "" {
		return fmt.Errorf("源序列 %s: %s", current.Name, current.Unsupported)
	}
	if current.Exhausted {
		return fmt.Errorf("源序列 %s 已耗尽", current.Name)
	}
	if repair.Hash(pgOracleSequenceDefinition(saved)) != repair.Hash(pgOracleSequenceDefinition(current)) {
		return fmt.Errorf("源序列 %s 的身份、定义或完整绑定已变化", current.Name)
	}
	return nil
}

func pgOracleSequenceDefinition(s source.PGSequence) source.PGSequence {
	s.LastValue, s.StartValue, s.IsCalled, s.Exhausted = 0, 0, false, false
	s.StartValueText = ""
	return s
}

// PGOracleRecoveryPlans refreshes source watermarks and binds them to the saved
// target identities after related sequence/trigger repairs finish. The caller
// supplies the final related objects, including their persisted identities.
func PGOracleRecoveryPlans(ctx context.Context, r source.Reader, w target.Writer, p repair.RecoveryPreview, related []repair.Object) ([]pgoracle.SequencePlan, error) {
	if !source.IsPGFamily(r.DBType()) || w.DBType() != "oracle" {
		return nil, nil
	}
	// Include successful original objects as well as just-executed repairs.
	ctx = WithPGOracleRecoveryEvidence(ctx, p.PGOracleSequences)
	p.PGOracleSequences = nil
	if err := preparePGOracleRecovery(ctx, r, w, &p, related); err != nil {
		return nil, err
	}
	return p.PGOracleSequences, nil
}
