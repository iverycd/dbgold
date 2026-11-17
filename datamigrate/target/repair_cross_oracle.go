package target

import (
	"context"
	"dbgold/datamigrate/repair"
	"fmt"
	"slices"
)

func (w *OracleWriter) executeCrossOracleSequenceRepair(ctx context.Context, obj repair.Object, out []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	if obj.Schema != w.schema || w.schema == "" {
		return out, fmt.Errorf("序列修复 Schema 与当前目标不一致")
	}
	if !obj.TrustedCrossOracleSequence() || len(out) != len(obj.Units) {
		return out, fmt.Errorf("序列修复必须使用完整冻结计划")
	}
	for i, u := range out {
		if u.SQL != obj.Units[i].SQL || u.OriginalSQL != obj.Units[i].OriginalSQL || u.ID != obj.Units[i].ID {
			return out, fmt.Errorf("序列修复不允许修改程序")
		}
		if u.Status == "unknown" || u.Status == "running" {
			return out, fmt.Errorf("请先核对结果未知的序列步骤")
		}
		if u.Status != "succeeded" && !u.Selected {
			return out, fmt.Errorf("请选择全部待处理序列步骤")
		}
	}
	if err := w.inspectCrossOracleSequence(ctx, *obj.Sequence); err != nil {
		return out, err
	}
	statements := w.dia.SequenceStatements(w.schema, *obj.Sequence, "")
	if len(statements) != len(out) {
		return out, fmt.Errorf("序列绑定步骤数不一致")
	}
	for i := range out {
		out[i].SQL = statements[i].SQL
		out[i].OriginalSQL = statements[i].SQL
		out[i].Status = "running"
	}
	if onStep != nil {
		if e := onStep(slices.Clone(out)); e != nil {
			return out, e
		}
	}
	err := w.createCrossOracleSequence(ctx, *obj.Sequence)
	for i := range out {
		if err == nil {
			out[i].Status = "succeeded"
			out[i].Selected = false
		} else {
			out[i].Status = repairErrorStatus(err)
			out[i].Error = err.Error()
		}
	}
	if onStep != nil {
		if e := onStep(slices.Clone(out)); e != nil {
			return out, e
		}
	}
	return out, err
}
