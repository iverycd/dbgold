package target

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
)

func executePGOracleSequenceRepair(ctx context.Context, w Writer, obj repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	out := slices.Clone(units)
	ow, ok := w.(*OracleWriter)
	if !ok {
		return out, fmt.Errorf("PG Oracle 序列修复必须使用完整、未编辑的冻结方案")
	}
	if err := ValidatePGOracleSequenceRepair(obj, units, w.DBType()); err != nil {
		return out, err
	}
	p := obj.PGOracleSequence
	if p.Schema != ow.schema {
		return out, fmt.Errorf("目标 schema 与序列修复对象不一致")
	}
	if err := ow.InspectPGOracleSequence(ctx, *p); err != nil {
		return out, err
	}
	if err := ow.ValidatePGOracleSequenceBindings(ctx, *p, false); err != nil {
		return out, err
	}
	exists := p.Identity != nil
	if obj.Kind == "trigger" {
		if p.Identity == nil {
			return out, fmt.Errorf("绑定触发器依赖的工具序列尚无持久化身份")
		}
		for _, b := range p.Bindings {
			if b.Trigger == obj.TargetName {
				var err error
				exists, err = ow.inspectPGOracleSequenceTrigger(ctx, *p, b)
				if err != nil {
					return out, err
				}
			}
		}
	}
	if out[0].Status == "succeeded" && !exists {
		return out, fmt.Errorf("已成功对象 %s 已不存在，禁止重放成功 CREATE", obj.TargetName)
	}
	persist := func() error {
		if onStep == nil {
			return nil
		}
		return onStep(slices.Clone(out))
	}
	calibration := false
	if obj.Kind == "sequence" && p.Identity != nil {
		statements, err := p.CalibrationStatements()
		if err != nil {
			return out, err
		}
		calibration = len(statements) == 1 && statements[0].SQL == out[0].SQL
	}
	if exists && (!calibration || out[0].Status == "succeeded") {
		// This is a read-only identity/definition proof, never CREATE OR REPLACE
		// and never a NEXTVAL that could advance an already-successful object.
		out[0].Status, out[0].Error, out[0].Selected = "succeeded", "", false
		return out, persist()
	}
	if !out[0].Selected {
		return out, fmt.Errorf("未选择执行步骤")
	}
	out[0].Status, out[0].Error = "running", ""
	if err := persist(); err != nil {
		out[0].Status = "pending"
		return out, fmt.Errorf("保存执行前状态失败，未执行 SQL: %w", err)
	}
	err := ow.ExecutePGOracleStatements(ctx, obj.Kind, obj.TargetName, []dialect.Statement{{SQL: out[0].SQL}})
	if err != nil {
		out[0].Status, out[0].Error = repairErrorStatus(err), err.Error()
		return out, errors.Join(err, persist())
	}
	if obj.Kind == "sequence" {
		identity, e := ow.PGOracleSequenceIdentity(ctx, *p)
		if e != nil {
			out[0].Status, out[0].Error = "unknown", e.Error()
			return out, errors.Join(fmt.Errorf("CREATE 已执行但序列身份无法确认: %w", e), persist())
		}
		// The caller's object shares this plan pointer. Its onStep serializes the
		// identity with the successful unit before another repair may proceed.
		p.Identity = identity
	}
	out[0].Status, out[0].Error, out[0].Selected = "succeeded", "", false
	if err := persist(); err != nil {
		out[0].Status, out[0].Error = "unknown", err.Error()
		return out, fmt.Errorf("SQL 已执行但保存对象身份/结果失败: %w", err)
	}
	return out, nil
}

func ValidatePGOracleSequenceRepair(obj repair.Object, units []repair.Unit, dbType string) error {
	if dbType != "oracle" || !obj.TrustedPGOracleSequence() || len(units) != 1 || units[0].ID != obj.Units[0].ID || units[0].SQL != obj.Units[0].SQL || units[0].OriginalSQL != obj.Units[0].OriginalSQL {
		return fmt.Errorf("PG Oracle 序列修复必须使用完整、未编辑的冻结方案")
	}
	return nil
}

// CalibratePGOracleRecovery retains the original generator and checks every
// frozen consumer before advancing it. CYCLE generators retain their actual
// position; no MAX+1 rewrite or diagnostic NEXTVAL is issued for a cycle.
func CalibratePGOracleRecovery(ctx context.Context, w Writer, table string, plans []pgoracle.SequencePlan) error {
	if len(plans) == 0 {
		return nil
	}
	ow, ok := w.(*OracleWriter)
	if !ok {
		return fmt.Errorf("PG 序列校准需要 Oracle Writer")
	}
	var ready []pgoracle.SequencePlan
	seen := map[string]bool{}
	for _, p := range plans {
		bound := false
		for _, b := range p.Bindings {
			bound = bound || b.Table == table
		}
		if !bound {
			continue
		}
		if seen[p.Schema+"\x00"+p.Name] {
			continue
		}
		seen[p.Schema+"\x00"+p.Name] = true
		if p.Identity == nil {
			return fmt.Errorf("序列 %s 缺少持久化身份，拒绝校准", p.Name)
		}
		if err := ow.InspectPGOracleSequence(ctx, p); err != nil {
			return err
		}
		if err := ow.ValidatePGOracleSequenceBindings(ctx, p, false); err != nil {
			return err
		}
		ready = append(ready, p)
	}
	for _, p := range ready {
		if p.Source.Cycle {
			continue
		}
		statement, err := pgOracleCalibrationSQL(p)
		if err != nil {
			return err
		}
		if err = ow.execStatements(ctx, []dialect.Statement{{SQL: statement}}); err != nil {
			return fmt.Errorf("序列 %s 向前校准失败: %w", p.Name, err)
		}
		if _, err = ow.PGOracleSequenceIdentity(ctx, p); err != nil {
			return fmt.Errorf("序列校准后定义复核失败: %w", err)
		}
	}
	return nil
}

func pgOracleCalibrationSQL(p pgoracle.SequencePlan) (string, error) {
	statements, err := p.CalibrationStatements()
	if err != nil {
		return "", err
	}
	if len(statements) != 1 {
		return "", fmt.Errorf("序列校准方案缺少完整单元")
	}
	return statements[0].SQL, nil
}
