package target

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

type oracleDamengManagedTrigger struct {
	name, sequence, column string
	onNull                 bool
}

// A marker is not sufficient: the complete generated body, event and timing
// must match, so recovery cannot replay arbitrary application trigger effects.
func (w *DaMengWriter) oracleDamengManagedTriggers(ctx context.Context, table string) ([]oracleDamengManagedTrigger, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT OWNER,TRIGGER_NAME,TRIGGER_TYPE,TRIGGERING_EVENT,WHEN_CLAUSE,TRIGGER_BODY FROM ALL_TRIGGERS WHERE TABLE_OWNER=? AND TABLE_NAME=? AND STATUS='ENABLED'`, w.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []oracleDamengManagedTrigger
	marker := regexp.MustCompile(`/\* DBGOLD_ORACLE_DAMENG:([^*]+) \*/`)
	assignment := regexp.MustCompile(`SELECT "((?:[^"]|"")+)"\."((?:[^"]|"")+)"\.NEXTVAL INTO :NEW\."((?:[^"]|"")+)" FROM DUAL;`)
	for rows.Next() {
		var owner, name, timing, event, body string
		var when sql.NullString
		if err = rows.Scan(&owner, &name, &timing, &event, &when, &body); err != nil {
			return nil, err
		}
		fail := func() error {
			return fmt.Errorf("目标表 %s 存在非本工具生成的触发器 %s，不能安全复制数据", table, name)
		}
		if owner != w.schema || timing != "BEFORE EACH ROW" || strings.TrimSpace(event) != "INSERT" || when.Valid && strings.TrimSpace(when.String) != "" {
			return nil, fail()
		}
		m, a := marker.FindStringSubmatch(body), assignment.FindStringSubmatch(body)
		if len(m) != 2 || len(a) != 4 || strings.ReplaceAll(a[1], `""`, `"`) != w.schema {
			return nil, fail()
		}
		seqName, column := strings.ReplaceAll(a[2], `""`, `"`), strings.ReplaceAll(a[3], `""`, `"`)
		if m[1] != seqName {
			return nil, fail()
		}
		matched, onlyNull := false, false
		for _, onNull := range []bool{false, true} {
			seq := source.SequenceInfo{StartValueText: "1", Native: &source.NativeSequenceInfo{Name: seqName, IncrementBy: "1", MinValue: "1", MaxValue: "9223372036854775807", CacheSize: "0", Bindings: []source.SequenceBinding{{TableName: table, ColumnName: column, Kind: "trigger", TriggerName: name, OnlyWhenNull: onNull}}}}
			ss := w.dia.SequenceStatements(w.schema, seq, "")
			if len(ss) != 2 {
				return nil, fail()
			}
			pos := strings.Index(ss[1].SQL, "\nBEGIN")
			if pos >= 0 && strings.Join(strings.Fields(body), " ") == strings.Join(strings.Fields(ss[1].SQL[pos:]), " ") {
				matched, onlyNull = true, onNull
				break
			}
		}
		if !matched {
			return nil, fail()
		}
		out = append(out, oracleDamengManagedTrigger{name: name, sequence: seqName, column: column, onNull: onlyNull})
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
	if err := dialect.ValidateOracleDamengSequence(seq); err != nil {
		return err
	}
	n := seq.Native
	var stepText, minText, maxText, currentText, cycle string
	if err := w.db.QueryRowContext(ctx, `SELECT INCREMENT_BY,MIN_VALUE,MAX_VALUE,LAST_NUMBER,CYCLE_FLAG FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?`, w.schema, n.Name).Scan(&stepText, &minText, &maxText, &currentText, &cycle); err != nil {
		return fmt.Errorf("读取已有序列 %s: %w", n.Name, err)
	}
	if stepText != n.IncrementBy || minText != n.MinValue || maxText != n.MaxValue || cycle != "N" || n.Cycle {
		return fmt.Errorf("已有序列 %s 的步长、边界或循环属性与源端不兼容", n.Name)
	}
	step, err := repairInteger(stepText)
	if err != nil {
		return err
	}
	current, err := repairInteger(currentText)
	if err != nil {
		return err
	}
	start, err := recoveryStart(seq)
	if err != nil {
		return err
	}
	bound := farther(new(big.Int).Set(current), start, step)
	for _, b := range n.Bindings {
		triggers, err := w.oracleDamengManagedTriggers(ctx, b.TableName)
		if err != nil {
			return err
		}
		matched := false
		for _, t := range triggers {
			if t.name == dialect.OracleDamengTriggerName(seq, b) && t.sequence == n.Name && t.column == b.ColumnName && t.onNull == b.OnlyWhenNull {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("序列 %s 的目标绑定 %s.%s 不存在或已改变", n.Name, b.TableName, b.ColumnName)
		}
		v := seq
		v.TableName, v.ColumnName = b.TableName, b.ColumnName
		extreme, err := recoveryExtreme(ctx, w, w.db, w.schema, v, step)
		if err != nil {
			return err
		}
		if extreme != nil {
			bound = farther(bound, afterExtreme(extreme, step), step)
		}
	}
	// Keep the existing sequence's arithmetic progression when the imported
	// maximum/minimum lies between two generated values.
	distance := new(big.Int).Sub(bound, current)
	if step.Sign() < 0 {
		distance.Neg(distance)
	}
	absStep := new(big.Int).Abs(step)
	q, rem := new(big.Int).QuoRem(distance, absStep, new(big.Int))
	if rem.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	next := new(big.Int).Add(current, new(big.Int).Mul(q, step))
	lo, _ := repairInteger(n.MinValue)
	hi, _ := repairInteger(n.MaxValue)
	if !next.IsInt64() || next.Cmp(lo) < 0 || next.Cmp(hi) > 0 {
		return fmt.Errorf("序列 %s 续值超出达梦或源序列边界", n.Name)
	}
	if next.Cmp(current) == 0 {
		return nil
	}
	return w.execStatements(ctx, []dialect.Statement{{SQL: "ALTER SEQUENCE " + w.dia.QualifyTable(w.schema, n.Name) + " CURRENT VALUE " + next.String()}})
}
