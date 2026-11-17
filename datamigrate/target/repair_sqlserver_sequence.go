package target

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

func IsSQLServerSequenceRepair(obj repair.Object, dbType string) bool {
	s := obj.Sequence
	return dbType == "sqlserver" && repairKind(obj.Kind) == "sequence" && s != nil &&
		(s.Native != nil || s.TableName != "" && s.ColumnName != "" && obj.TargetName == s.TableName+"."+s.ColumnName)
}

// ValidateSQLServerSequenceRepair accepts the complete saved object, without
// executing edited SQL. The Writer rechecks target ownership and live progress.
func ValidateSQLServerSequenceRepair(obj repair.Object, units []repair.Unit, dbType string) error {
	if !IsSQLServerSequenceRepair(obj, dbType) || obj.Schema == "" || obj.Sequence.Unsupported != "" {
		return fmt.Errorf("缺少可自动执行的 SQL Server 序列修复计划")
	}
	s := obj.Sequence
	if s.Native != nil {
		if obj.TargetName != s.Native.Name {
			return fmt.Errorf("SQL Server 序列目标名称与原记录不一致")
		}
		if _, err := dialect.ValidateSQLServerNativeSequence(*s); err != nil {
			return err
		}
	} else if obj.Table != s.TableName {
		return fmt.Errorf("SQL Server IDENTITY 目标表与原记录不一致")
	}
	if len(units) == 0 || len(units) != len(obj.Units) {
		return fmt.Errorf("SQL Server 序列修复需要保留全部冻结步骤")
	}
	for i, unit := range units {
		original := obj.Units[i]
		if unit.ID != original.ID || unit.SQL == "" || unit.SQL != original.OriginalSQL || unit.OriginalSQL != original.OriginalSQL {
			return fmt.Errorf("SQL Server 序列修复步骤不支持编辑或重排")
		}
	}
	return nil
}

func (w *SQLServerWriter) executeSQLServerSequenceRepair(ctx context.Context, obj repair.Object, out []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	if err := ValidateSQLServerSequenceRepair(obj, out, w.DBType()); err != nil {
		return out, err
	}
	selected := []int{}
	for i := range out {
		switch out[i].Status {
		case "succeeded":
			out[i].Selected = false
			continue
		case "unknown", "running":
			return out, fmt.Errorf("存在执行结果未知的序列步骤，请先核对目标状态")
		}
		if !out[i].Selected {
			return out, fmt.Errorf("请选择全部待处理的 SQL Server 序列步骤")
		}
		selected = append(selected, i)
	}
	if len(selected) == 0 {
		return out, fmt.Errorf("未选择待处理的 SQL Server 序列步骤")
	}
	before := slices.Clone(out)
	for _, i := range selected {
		out[i].Status, out[i].Error = "running", ""
	}
	if onStep != nil {
		if err := onStep(slices.Clone(out)); err != nil {
			return before, fmt.Errorf("保存序列修复执行前状态: %w", err)
		}
	}
	s := *obj.Sequence
	var err error
	if s.Native != nil {
		err = w.createSQLServerNativeSequence(ctx, s)
	} else {
		var actual sqlServerIdentityState
		actual, err = w.sqlServerRepairIdentityState(ctx, s)
		if err == nil {
			stmts := w.dia.SQLServerIdentityCalibrationStatements(w.schema, s, actual.mapped, actual.step.String())
			if len(stmts) == 0 {
				err = fmt.Errorf("SQL Server IDENTITY 修复缺少兼容的类型或起点")
			} else {
				err = w.execStatements(ctx, stmts)
			}
		}
	}
	for _, i := range selected {
		if err == nil {
			out[i].Status, out[i].Selected = "succeeded", false
		} else {
			out[i].Status, out[i].Error = repairErrorStatus(err), err.Error()
		}
	}
	if onStep != nil {
		if persistErr := onStep(slices.Clone(out)); persistErr != nil {
			for _, i := range selected {
				out[i].Status = "unknown"
			}
			err = errors.Join(err, fmt.Errorf("保存序列修复执行结果: %w", persistErr))
		}
	}
	return out, err
}

type sqlServerIdentityState struct {
	mapped     string
	step, next *big.Int
}

func (w *SQLServerWriter) sqlServerRepairIdentityState(ctx context.Context, s source.SequenceInfo) (sqlServerIdentityState, error) {
	name := w.dia.QualifyTable(w.schema, s.TableName)
	var typ, rawStep, seed string
	var p, scale int64
	var last sql.NullString
	q := `SELECT t.name,c.precision,c.scale,CONVERT(varchar(50),c.increment_value),CONVERT(varchar(50),c.seed_value),CONVERT(varchar(50),c.last_value) FROM sys.identity_columns c JOIN sys.types t ON t.user_type_id=c.user_type_id WHERE c.object_id=OBJECT_ID(@p1,N'U') AND c.name=@p2`
	err := w.db.QueryRowContext(ctx, q, name, s.ColumnName).Scan(&typ, &p, &scale, &rawStep, &seed, &last)
	if err == sql.ErrNoRows {
		return sqlServerIdentityState{}, fmt.Errorf("目标 IDENTITY 列 %s.%s 不存在", name, s.ColumnName)
	}
	if err != nil {
		return sqlServerIdentityState{}, err
	}
	mapped := strings.ToUpper(typ)
	switch mapped {
	case "TINYINT", "SMALLINT", "INT", "BIGINT":
	case "DECIMAL", "NUMERIC":
		if scale != 0 || p < 1 || p > 38 {
			return sqlServerIdentityState{}, fmt.Errorf("目标 IDENTITY 列不是兼容的整数类型")
		}
		mapped = fmt.Sprintf("DECIMAL(%d,0)", p)
	default:
		return sqlServerIdentityState{}, fmt.Errorf("目标 IDENTITY 列不是兼容的整数类型")
	}
	want := "1"
	if s.IdentityInfo != nil && s.IdentityInfo.Increment != 0 {
		want = strconv.FormatInt(s.IdentityInfo.Increment, 10)
	}
	if rawStep != want {
		return sqlServerIdentityState{}, fmt.Errorf("目标 IDENTITY 步长为 %s，原计划为 %s", rawStep, want)
	}
	step, err := repairInteger(rawStep)
	if err != nil {
		return sqlServerIdentityState{}, err
	}
	next, err := repairInteger(seed)
	if last.Valid {
		next, err = repairInteger(last.String)
		if err == nil {
			next.Add(next, step)
		}
	}
	return sqlServerIdentityState{mapped: mapped, step: step, next: next}, err
}

func (w *SQLServerWriter) verifySQLServerSequence(ctx context.Context, s source.SequenceInfo) error {
	if s.Unsupported != "" {
		return fmt.Errorf("%s", s.Unsupported)
	}
	if s.Native == nil {
		state, err := w.sqlServerRepairIdentityState(ctx, s)
		if err != nil {
			return err
		}
		start, err := recoveryStart(s)
		if err != nil {
			return err
		}
		extreme, err := recoveryExtreme(ctx, w, w.db, w.schema, s, state.step)
		if err != nil {
			return err
		}
		if farther(state.next, start, state.step).Cmp(state.next) != 0 || extreme != nil && farther(state.next, afterExtreme(extreme, state.step), state.step).Cmp(state.next) != 0 {
			return fmt.Errorf("目标 IDENTITY 仍需向前校准")
		}
		return nil
	}
	spec, err := dialect.ValidateSQLServerNativeSequence(s)
	if err != nil {
		return err
	}
	qualified := w.dia.QualifyTable(w.schema, s.Native.Name)
	state, err := sqlServerSequenceState(ctx, w.db, qualified)
	if err != nil {
		return err
	}
	if state.id == 0 {
		return fmt.Errorf("目标序列 %s 不存在", qualified)
	}
	if err := state.validate(spec, qualified); err != nil {
		return err
	}
	bound := new(big.Int).Set(spec.Start)
	verified := false
	for _, b := range s.Native.Bindings {
		if b.Kind == "ownership" {
			continue
		}
		ok, err := w.sqlServerSequenceBinding(ctx, w.db, b, state.id, s.Native.Name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("目标列 %s.%s 缺少指向 %s 的直接默认值", b.TableName, b.ColumnName, qualified)
		}
		verified = true
		extreme, err := recoveryExtreme(ctx, w, w.db, w.schema, source.SequenceInfo{TableName: b.TableName, ColumnName: b.ColumnName}, spec.Increment)
		if err != nil {
			return err
		}
		if extreme != nil {
			bound = farther(bound, afterExtreme(extreme, spec.Increment), spec.Increment)
		}
	}
	if !verified && !state.marker.Valid {
		return fmt.Errorf("目标独立序列 %s 缺少迁移所有权标记", qualified)
	}
	current, _, err := state.next(ctx, w.db, spec.Increment)
	if err != nil {
		return err
	}
	if farther(current, bound, spec.Increment).Cmp(current) != 0 {
		return fmt.Errorf("目标序列 %s 仍需向前校准", qualified)
	}
	return nil
}
