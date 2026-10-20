package target

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/source"
)

type oracleDamengManagedTrigger struct {
	name, sequence, column string
	onNull                 bool
}

// A marker is not sufficient: the complete generated body, event and timing
// must match, so recovery cannot replay arbitrary application trigger effects.
func (w *DaMengWriter) oracleDamengManagedTriggers(ctx context.Context, table string) ([]oracleDamengManagedTrigger, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT OWNER,TRIGGER_NAME,TABLE_OWNER,TABLE_NAME,TRIGGERING_TYPE,TRIGGERING_EVENT,STATUS,DESCRIPTION,TRIGGER_BODY FROM ALL_TRIGGERS WHERE TABLE_OWNER=? AND TABLE_NAME=? AND STATUS IN ('ENABLED','Y')`, w.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []oracleDamengManagedTrigger
	marker := regexp.MustCompile(`/\* DBGOLD_ORACLE_DAMENG:([^*]+) \*/`)
	assignment := regexp.MustCompile(`SELECT "((?:[^"]|"")+)"\."((?:[^"]|"")+)"\.NEXTVAL INTO :NEW\."((?:[^"]|"")+)" FROM DUAL;`)
	for rows.Next() {
		actual, err := scanOracleDamengSequenceTrigger(rows)
		if err != nil {
			return nil, err
		}
		fail := func() error {
			return fmt.Errorf("目标表 %s 存在非本工具生成的触发器 %s，不能安全复制数据", table, actual.name)
		}
		m, a := marker.FindStringSubmatch(actual.body), assignment.FindStringSubmatch(actual.body)
		if len(m) != 2 || len(a) != 4 || strings.ReplaceAll(a[1], `""`, `"`) != w.schema {
			return nil, fail()
		}
		seqName, column := strings.ReplaceAll(a[2], `""`, `"`), strings.ReplaceAll(a[3], `""`, `"`)
		if m[1] != seqName {
			return nil, fail()
		}
		matched, onlyNull := false, false
		for _, onNull := range []bool{false, true} {
			seq := source.SequenceInfo{StartValueText: "1", Native: &source.NativeSequenceInfo{Name: seqName, IncrementBy: "1", MinValue: "1", MaxValue: "9223372036854775807", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: table, ColumnName: column, Kind: "trigger", TriggerName: actual.name, OnlyWhenNull: onNull}}}}
			ss := w.dia.SequenceStatements(w.schema, seq, "")
			if len(ss) != 2 {
				return nil, fail()
			}
			if w.validateOracleDamengSequenceTrigger(actual, seq.Native.Bindings[0], ss[1].SQL) == nil {
				matched, onlyNull = true, onNull
				break
			}
		}
		if !matched {
			return nil, fail()
		}
		out = append(out, oracleDamengManagedTrigger{name: actual.name, sequence: seqName, column: column, onNull: onlyNull})
	}
	return out, rows.Err()
}

func (w *DaMengWriter) OnlyManagedOracleDamengTriggers(ctx context.Context, table string) (bool, error) {
	triggers, err := w.oracleDamengManagedTriggers(ctx, table)
	if err != nil {
		return false, err
	}
	for _, t := range triggers {
		if !t.onNull {
			return false, fmt.Errorf("目标触发器 %s 会覆盖显式编号，不能在启用状态下补迁数据", t.name)
		}
	}
	return len(triggers) > 0, nil
}

func (w *DaMengWriter) ValidateOracleDamengLoadTriggers(ctx context.Context, info *source.TableDDLInfo) error {
	triggers, err := w.oracleDamengManagedTriggers(ctx, strings.ToUpper(info.TableName))
	if err != nil {
		return err
	}
	for _, t := range triggers {
		if !t.onNull {
			return fmt.Errorf("目标触发器 %s 会覆盖显式编号；仅数据模式已阻止，未禁用触发器", t.name)
		}
		found := false
		for _, c := range info.Columns {
			if strings.ToUpper(c.Name) == t.column {
				found = true
				if c.IsNullable {
					return fmt.Errorf("源列 %s 可为 NULL，目标触发器 %s 可能改变原值，不能仅迁数据", c.Name, t.name)
				}
			}
		}
		if !found {
			return fmt.Errorf("目标触发器 %s 的绑定列不在源表中", t.name)
		}
	}
	return nil
}

// Advance the existing native generator without consuming NEXTVAL, recreating
// it, or changing its bindings. LAST_NUMBER can introduce safe cache gaps.
func (w *DaMengWriter) CalibrateOracleDameng(ctx context.Context, seq source.SequenceInfo) error {
	plan, err := w.planOracleDamengSequence(ctx, seq)
	if err != nil {
		return err
	}
	if !plan.sequenceExists || len(plan.missingTriggers) != 0 {
		return OracleDamengSequencePreflight(fmt.Errorf("序列或目标绑定不存在；仅数据及补迁校准不创建对象，请先修复序列和触发器"))
	}
	return w.execStatements(ctx, plan.statements)
}
