package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

type PartitionIntervalMaterializer interface {
	MaterializePartitionIntervals(context.Context, *partition.Plan) error
}

// MaterializePartitionIntervals recreates existing empty automatic leaves using
// native DM routing. The temporary row is always rolled back; the native leaf
// allocation survives. Neither source routines nor target triggers may run.
func (w *DaMengWriter) MaterializePartitionIntervals(ctx context.Context, plan *partition.Plan) (retErr error) {
	r, err := validateIntervalRecipe(w.schema, plan)
	if err != nil {
		return err
	}
	marker := partition.IntervalMaterializationSQL(plan)
	if err = repair.Observe(ctx, marker, "running", nil); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, repair.Observe(ctx, marker, repairErrorStatus(retErr), retErr))
		}
	}()
	actual, err := w.InspectPartitionTable(ctx, r.Table)
	if err != nil {
		return err
	}
	matched, err := matchIntervalSubset(plan.Target, actual)
	if err != nil {
		return err
	}
	// A completed materialization is read-only on retry, including after data
	// and post-DDL were installed. Never touch existing business data.
	if partition.Equivalent(plan.Target, actual) == nil {
		return repair.Observe(ctx, marker, "succeeded", nil)
	}
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	qualified := q(r.Schema) + "." + q(r.Table)
	var count int64
	if err = w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qualified).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("INTERVAL 空叶物化要求目标根表为空；现有数据禁止触碰")
	}
	// A repaired table may already have root or physical-leaf triggers and
	// constraints. They can call user code, so even disabled ones are blocked.
	physical := `(TABLE_NAME=? OR TABLE_NAME IN (SELECT o.NAME FROM SYS.SYSHPARTTABLEINFO h JOIN SYS.SYSOBJECTS o ON o.ID=h.PART_TABLE_ID WHERE h.BASE_TABLE_ID=(SELECT b.ID FROM SYS.SYSOBJECTS b JOIN SYS.SYSOBJECTS s ON s.ID=b.SCHID WHERE s.NAME=? AND s.TYPE$='SCH' AND b.NAME=? AND b.SUBTYPE$='UTAB')))`
	for _, catalog := range []struct{ view, owner string }{{"ALL_TRIGGERS", "TABLE_OWNER"}, {"ALL_CONSTRAINTS", "OWNER"}} {
		if err = w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+catalog.view+" WHERE "+catalog.owner+"=? AND "+physical, r.Schema, r.Table, r.Schema, r.Table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("INTERVAL 空叶物化拒绝已有触发器/业务约束（%s），请先完成安全的结构修复", catalog.view)
		}
	}
	var ddl string
	if err = w.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?)`, r.Table, r.Schema).Scan(&ddl); err != nil {
		return err
	}
	if err = intervalSafeDefinition(ddl, r.Columns); err != nil {
		return err
	}
	columns := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		columns[i] = q(c)
	}
	for _, seed := range r.Seeds {
		got := matched[seed.PartitionName]
		if got == "" {
			values := make([]string, len(r.Values))
			for i, v := range r.Values {
				if i == r.KeyIndex {
					v = seed.Value
				}
				values[i], err = partition.IntervalLiteral(v)
				if err != nil {
					return err
				}
			}
			statement := "INSERT INTO " + qualified + " (" + strings.Join(columns, ",") + ") VALUES (" + strings.Join(values, ",") + ")"
			seedConn, e := w.db.Conn(ctx)
			if e != nil {
				return e
			}
			tx, e := seedConn.BeginTx(ctx, nil)
			if e != nil {
				_ = seedConn.Close()
				return e
			}
			_, insertErr := tx.ExecContext(ctx, statement)
			rollbackErr := tx.Rollback()
			if rollbackErr != nil {
				_ = seedConn.Raw(func(any) error { return driver.ErrBadConn })
			}
			_ = seedConn.Close()
			if insertErr != nil || rollbackErr != nil {
				return fmt.Errorf("INTERVAL 临时行插入/回滚未完全确认: %w", errors.Join(insertErr, rollbackErr))
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			actual, err = w.InspectPartitionTable(ctx, r.Table)
			if err != nil {
				return err
			}
			matched, err = matchIntervalSubset(plan.Target, actual)
			if err != nil {
				return err
			}
			got = matched[seed.PartitionName]
			if got == "" {
				return fmt.Errorf("INTERVAL 原生种子未产生预期边界 %s", seed.PartitionName)
			}
		}
		if got != seed.PartitionName {
			if _, err = w.db.ExecContext(ctx, "ALTER TABLE "+qualified+" RENAME PARTITION "+q(got)+" TO "+q(seed.PartitionName)); err != nil {
				return err
			}
			matched[seed.PartitionName] = seed.PartitionName
		}
	}
	if err = w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qualified).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("INTERVAL 物化结束时根表非空，不能声明临时行已清除")
	}
	actual, err = w.InspectPartitionTable(ctx, r.Table)
	if err != nil {
		return err
	}
	if err = partition.Equivalent(plan.Target, actual); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return repair.Observe(ctx, marker, "succeeded", nil)
}

func validateIntervalRecipe(schema string, plan *partition.Plan) (*partition.IntervalMaterialization, error) {
	if plan == nil || plan.Interval == nil || plan.Target == nil || plan.Target.Interval == nil {
		return nil, fmt.Errorf("缺少冻结 INTERVAL 物化计划")
	}
	r := plan.Interval
	if schema == "" || r.Schema != schema || r.Table == "" || len(plan.Target.Keys) != 1 || r.KeyIndex < 0 || r.KeyIndex >= len(r.Columns) || len(r.Columns) != len(r.Values) || len(r.Seeds) == 0 || r.Columns[r.KeyIndex] != plan.Target.Keys[0].Column {
		return nil, fmt.Errorf("INTERVAL 物化作用域或列映射不完整")
	}
	columns := map[string]bool{}
	for i, c := range r.Columns {
		if c == "" || columns[c] {
			return nil, fmt.Errorf("INTERVAL 物化列名为空或重复")
		}
		columns[c] = true
		if _, err := partition.IntervalLiteral(r.Values[i]); err != nil {
			return nil, err
		}
	}
	want := map[string]source.Partition{}
	for _, p := range plan.Target.Partitions {
		if p.Schema != schema {
			return nil, fmt.Errorf("INTERVAL 叶不属于目标 schema")
		}
		if p.IsInterval {
			want[p.Name] = p
		}
	}
	if len(want) != len(r.Seeds) {
		return nil, fmt.Errorf("INTERVAL 物化种子数量不匹配")
	}
	for _, seed := range r.Seeds {
		p, ok := want[seed.PartitionName]
		if !ok || len(p.Lower) != 1 {
			return nil, fmt.Errorf("INTERVAL 物化种子不属于冻结叶")
		}
		a, e := partition.IntervalLiteral(seed.Value)
		if e != nil {
			return nil, e
		}
		b, e := partition.IntervalLiteral(p.Lower[0])
		if e != nil || a != b {
			return nil, fmt.Errorf("INTERVAL 物化种子偏离冻结下界")
		}
		delete(want, seed.PartitionName)
	}
	return r, nil
}

func matchIntervalSubset(want, actual *source.PartitionSpec) (map[string]string, error) {
	if actual == nil || want == nil {
		return nil, fmt.Errorf("INTERVAL 根表不存在")
	}
	probe := source.ClonePartitionSpec(actual)
	expected := source.ClonePartitionSpec(want)
	expected.Partitions = nil
	matched := map[string]string{}
	for i, got := range actual.Partitions {
		found := false
		for _, p := range want.Partitions {
			if !got.IsInterval && got.Name != p.Name || got.IsInterval != p.IsInterval {
				continue
			}
			left := source.ClonePartitionSpec(want)
			right := source.ClonePartitionSpec(want)
			left.Partitions = []source.Partition{p}
			copy := got
			copy.Name = p.Name
			copy.Ordinal = p.Ordinal
			right.Partitions = []source.Partition{copy}
			if partition.Equivalent(left, right) != nil {
				continue
			}
			if matched[p.Name] != "" {
				return nil, fmt.Errorf("INTERVAL 目标存在重复边界")
			}
			matched[p.Name] = got.Name
			probe.Partitions[i].Name = p.Name
			probe.Partitions[i].Ordinal = p.Ordinal
			expected.Partitions = append(expected.Partitions, p)
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("INTERVAL 目标分区 %s 不属于冻结的原生范围", got.Name)
		}
	}
	for _, p := range want.Partitions {
		if !p.IsInterval && matched[p.Name] == "" {
			return nil, fmt.Errorf("INTERVAL 初始分区 %s 缺失", p.Name)
		}
	}
	if err := partition.Equivalent(expected, probe); err != nil {
		return nil, err
	}
	return matched, nil
}

func intervalSafeDefinition(ddl string, columns []string) error {
	ts, e := sqlsyntax.Lex(ddl)
	if e != nil {
		return e
	}
	open := -1
	for i, t := range ts {
		if t.Is("(") {
			open = i
			break
		}
	}
	if open < 0 {
		return fmt.Errorf("INTERVAL 目标列定义缺失")
	}
	depth := 1
	names := map[string]bool{}
	start := true
	for i := open + 1; i < len(ts) && depth > 0; i++ {
		t := ts[i]
		if t.Is("(") {
			depth++
		}
		if t.Is(")") {
			depth--
			if depth == 0 {
				break
			}
		}
		if start {
			if t.Kind != 'q' && t.Kind != 'i' {
				return fmt.Errorf("INTERVAL 目标列声明不可解析")
			}
			name := t.Value
			if t.Kind == 'i' {
				name = strings.ToUpper(name)
			}
			names[name] = true
			start = false
		}
		if t.Is("IDENTITY") || t.Is("GENERATED") || t.Is("CHECK") || t.Is("REFERENCES") || t.Is("AS") {
			return fmt.Errorf("INTERVAL 空叶物化拒绝生成列、自增或业务表达式")
		}
		if t.Is("ON") && i+1 < len(ts) && (ts[i+1].Is("NULL") || ts[i+1].Is("UPDATE")) {
			return fmt.Errorf("INTERVAL 空叶物化拒绝 DEFAULT ON NULL 或自动更新表达式")
		}
		if depth == 1 && t.Is(",") {
			start = true
		}
	}
	if depth != 0 || len(names) != len(columns) {
		return fmt.Errorf("INTERVAL 目标列范围与冻结计划不同")
	}
	for _, c := range columns {
		if !names[c] {
			return fmt.Errorf("INTERVAL 目标缺少冻结列 %s", c)
		}
	}
	return nil
}
