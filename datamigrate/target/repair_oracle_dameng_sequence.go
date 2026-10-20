package target

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

type oracleDamengRepairUnit struct {
	kind  string
	value *big.Int
}

// Generated units remain saved, reviewable SQL. Only exact generated CREATEs
// and numeric CURRENT VALUE statements may use target-aware replay semantics.
func (w *DaMengWriter) oracleDamengGeneratedRepairUnit(seq source.SequenceInfo, sqlText string) (oracleDamengRepairUnit, bool) {
	generated := w.dia.SequenceStatements(w.schema, seq, "")
	for i, statement := range generated {
		if statement.SQL == sqlText {
			if i == 0 {
				start, err := recoveryStart(seq)
				return oracleDamengRepairUnit{kind: "sequence", value: start}, err == nil
			}
			return oracleDamengRepairUnit{kind: "trigger"}, true
		}
	}
	qualified := w.dia.QualifyTable(w.schema, seq.Native.Name)
	if prefix := "ALTER SEQUENCE " + qualified + " CURRENT VALUE "; strings.HasPrefix(sqlText, prefix) {
		value, err := repairInteger(strings.TrimPrefix(sqlText, prefix))
		if err == nil && value.IsInt64() && sqlText == prefix+value.String() {
			return oracleDamengRepairUnit{kind: "alter", value: value}, true
		}
	}
	prefix := "CREATE SEQUENCE " + qualified + " START WITH "
	if strings.HasPrefix(sqlText, prefix) {
		start, _, ok := strings.Cut(strings.TrimPrefix(sqlText, prefix), " INCREMENT BY ")
		if ok {
			candidate := seq
			candidate.StartValueText = start
			ss := w.dia.SequenceStatements(w.schema, candidate, "")
			if len(ss) > 0 && ss[0].SQL == sqlText {
				value, err := recoveryStart(candidate)
				return oracleDamengRepairUnit{kind: "sequence", value: value}, err == nil
			}
		}
	}
	return oracleDamengRepairUnit{}, false
}

func (w *DaMengWriter) executeOracleDamengSequenceRepair(ctx context.Context, obj repair.Object, out []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	preflight := func(err error) ([]repair.Unit, error) { return out, OracleDamengSequencePreflight(err) }
	seq, err := dialect.NormalizeOracleDamengSequence(*obj.Sequence)
	if err != nil {
		return preflight(err)
	}
	selected := []int{}
	classified := map[int]oracleDamengRepairUnit{}
	selectedSQL := map[string]bool{}
	selectedCreates := 0
	for i, unit := range out {
		if unit.Status == "unknown" || unit.Status == "running" {
			return preflight(fmt.Errorf("存在执行结果未知的序列步骤，不允许自动重放"))
		}
		kind, ok := w.oracleDamengGeneratedRepairUnit(seq, unit.SQL)
		if !ok {
			return preflight(fmt.Errorf("序列步骤不是当前生成器的 SQL，请审阅后按手工 SQL 保存，或刷新序列修复方案"))
		}
		classified[i] = kind
		if unit.Status == "succeeded" {
			out[i].Selected = false
			continue
		}
		if unit.Selected {
			if selectedSQL[unit.SQL] {
				return preflight(fmt.Errorf("序列计划包含重复的执行步骤，请刷新序列修复方案"))
			}
			selectedSQL[unit.SQL] = true
			if kind.kind == "sequence" {
				selectedCreates++
				if selectedCreates > 1 {
					return preflight(fmt.Errorf("序列计划包含多个创建序列步骤，请刷新序列修复方案"))
				}
			}
			selected = append(selected, i)
		}
	}
	if len(selected) == 0 {
		return preflight(fmt.Errorf("未选择执行步骤"))
	}
	plan, err := w.planOracleDamengSequence(ctx, seq)
	if err != nil {
		return out, err
	}
	lo, _ := repairInteger(seq.Native.MinValue)
	hi, _ := repairInteger(seq.Native.MaxValue)
	reused := map[int]bool{}
	sequenceAction := -1
	triggerAction := -1
	plannedCurrent := new(big.Int).Set(plan.current)
	for _, i := range selected {
		unit := classified[i]
		switch unit.kind {
		case "sequence":
			if plan.sequenceExists {
				reused[i] = true
				continue
			}
			if !validReviewedSequenceNext(unit.value, plan.next, plan.current, plan.step, lo, hi) {
				return preflight(fmt.Errorf("序列起值计划已变化，请刷新并保存序列修复方案"))
			}
			sequenceAction = i
		case "alter":
			if !plan.sequenceExists {
				return preflight(fmt.Errorf("目标序列尚不存在，不能执行续号校准"))
			}
			if farther(plannedCurrent, unit.value, plan.step).Cmp(plannedCurrent) == 0 {
				reused[i] = true
				continue
			}
			if !validReviewedSequenceNext(unit.value, plan.next, plannedCurrent, plan.step, lo, hi) {
				return preflight(fmt.Errorf("序列续号计划已变化，请刷新并保存序列修复方案"))
			}
			sequenceAction = i
			plannedCurrent.Set(unit.value)
		case "trigger":
			needed := false
			for _, statement := range plan.statements {
				needed = needed || statement.SQL == out[i].SQL
			}
			if !needed {
				reused[i] = true
				continue
			}
			if triggerAction < 0 {
				triggerAction = i
			}
		}
	}
	needsSequenceAction := !plan.sequenceExists || plan.next.Cmp(plan.current) != 0
	if needsSequenceAction && sequenceAction < 0 {
		return preflight(fmt.Errorf("需要先创建或向前校准序列，请刷新并选择相应的序列修复步骤"))
	}
	if triggerAction >= 0 && sequenceAction > triggerAction {
		return preflight(fmt.Errorf("序列创建或校准必须先于绑定触发器，请刷新序列修复方案"))
	}
	persist := func() error {
		if onStep == nil {
			return nil
		}
		return onStep(append([]repair.Unit(nil), out...))
	}
	for _, i := range selected {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if reused[i] {
			previous := out[i]
			out[i].Status, out[i].Error, out[i].Selected = "succeeded", "", false
			if err = persist(); err != nil {
				out[i] = previous
				return out, fmt.Errorf("保存序列复用检查结果失败，未执行该步骤: %w", err)
			}
			continue
		}
		out[i].Status, out[i].Error = "running", ""
		if err = persist(); err != nil {
			out[i].Status = "pending"
			return out, fmt.Errorf("保存执行前状态失败，未执行该步骤: %w", err)
		}
		_, err = w.db.ExecContext(ctx, out[i].SQL)
		if err != nil {
			out[i].Status, out[i].Error = repairErrorStatus(err), err.Error()
			return out, errors.Join(err, persist())
		}
		out[i].Status, out[i].Selected = "succeeded", false
		if err = persist(); err != nil {
			out[i].Status, out[i].Error = "unknown", err.Error()
			return out, fmt.Errorf("SQL 已执行，但保存结果失败: %w", err)
		}
	}
	return out, nil
}

func validReviewedSequenceNext(value, safe, current, step, lo, hi *big.Int) bool {
	if !value.IsInt64() || value.Cmp(lo) < 0 || value.Cmp(hi) > 0 || farther(safe, value, step).Cmp(value) != 0 {
		return false
	}
	return new(big.Int).Rem(new(big.Int).Sub(value, current), step).Sign() == 0
}
