package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

type nativeIntervalPart struct{ name, high, space, physical string }

func (w *DaMengNativeWriter) intervalPlan(object damengnative.Object, unit damengnative.Unit) (*damengnative.IntervalPartitionPlan, error) {
	p := object.Interval
	if object.Schema != w.schema || object.Kind != "table" || p == nil || p.Column == "" || p.EmptyTouch == nil || p.EmptyTouch.Table != object.Name || len(p.Partitions) == 0 || unit.Phase != "interval_prepare" || unit.SQL != damengnative.IntervalPartitionSQL(w.schema, object.Name, p) {
		return nil, fmt.Errorf("间隔分区执行单元不属于目标冻结表")
	}
	found := false
	for _, u := range object.Units {
		found = found || u.Phase == unit.Phase && u.SQL == unit.SQL
	}
	if !found {
		return nil, fmt.Errorf("间隔分区单元不在冻结清单中")
	}
	names := map[string]bool{}
	columns := map[string]bool{}
	mandatory := map[string]bool{}
	for _, column := range p.EmptyTouch.Columns {
		columns[column.Name] = true
		mandatory[column.Name] = column.ValueSQL != ""
	}
	for _, part := range p.Partitions {
		if part.Name == "" || part.HighSQL == "" || names[part.Name] {
			return nil, fmt.Errorf("间隔分区原生清单不完整或重复")
		}
		names[part.Name] = true
		values := map[string]bool{}
		for _, value := range part.Values {
			if !columns[value.Column] || value.SQL == "" || values[value.Column] || part.SeedSQL == "" || value.SQL == "NULL" && mandatory[value.Column] {
				return nil, fmt.Errorf("间隔分区仿射种子不属于冻结可写列")
			}
			values[value.Column] = true
		}
	}
	return p, nil
}

func nativeReadIntervalParts(ctx context.Context, conn *sql.Conn, schema, table string) ([]nativeIntervalPart, error) {
	rows, err := conn.QueryContext(ctx, `SELECT p.PARTITION_NAME,p.HIGH_VALUE,p.TABLESPACE_NAME,o.NAME FROM ALL_TAB_PARTITIONS p JOIN SYS.SYSOBJECTS b ON b.NAME=p.TABLE_NAME AND b.SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=p.TABLE_OWNER) JOIN SYS.SYSHPARTTABLEINFO h ON h.BASE_TABLE_ID=b.ID AND h.PARTITION_NAME=p.PARTITION_NAME JOIN SYS.SYSOBJECTS o ON o.ID=h.PART_TABLE_ID WHERE p.TABLE_OWNER=? AND p.TABLE_NAME=? ORDER BY p.PARTITION_POSITION`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []nativeIntervalPart
	for rows.Next() {
		var p nativeIntervalPart
		var high, space sql.NullString
		if err = rows.Scan(&p.name, &high, &space, &p.physical); err != nil {
			return nil, err
		}
		if !high.Valid {
			return nil, fmt.Errorf("目标间隔边界不可读取")
		}
		p.high, p.space = high.String, space.String
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

func intervalHighEqual(a, b string) bool { return nativeDefinitionsEqual(a, b) }

func (w *DaMengNativeWriter) RestoreDamengNativeIntervalPartitions(ctx context.Context, object damengnative.Object, unit damengnative.Unit) (retErr error) {
	p, err := w.intervalPlan(object, unit)
	if err != nil {
		return err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	if err = repair.Observe(ctx, unit.SQL, "running", nil); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, repair.Observe(ctx, unit.SQL, repairErrorStatus(retErr), retErr))
		}
	}()
	// A lost response may follow partition allocation but precede attribute
	// cleanup. Reconcile the complete stage definition before skipping any
	// existing partition. Use a proved existing automatic seed, never zero.
	restored, err := nativeIntervalStageRestored(ctx, conn, w.schema, p.EmptyTouch)
	if err != nil {
		return err
	}
	if !restored {
		recovered := false
		for _, part := range p.Partitions {
			if part.SeedSQL != "" {
				if err = nativeIntervalTouch(ctx, conn, w.schema, p, part); err != nil {
					return err
				}
				recovered = true
				break
			}
		}
		if !recovered {
			return fmt.Errorf("间隔分区没有可恢复临时属性的冻结键值")
		}
	}
	for _, want := range p.Partitions {
		actual, e := nativeReadIntervalParts(ctx, conn, w.schema, object.Name)
		if e != nil {
			return e
		}
		match := -1
		for i, got := range actual {
			if got.name == want.Name && !intervalHighEqual(got.high, want.HighSQL) {
				return fmt.Errorf("目标同名分区 %s 边界不同", want.Name)
			}
			if intervalHighEqual(got.high, want.HighSQL) {
				if match >= 0 {
					return fmt.Errorf("间隔分区边界不能唯一匹配")
				}
				match = i
			}
		}
		if match < 0 {
			if want.SeedSQL == "" {
				return fmt.Errorf("目标缺少初始间隔分区 %s", want.Name)
			}
			if err = nativeIntervalTouch(ctx, conn, w.schema, p, want); err != nil {
				return fmt.Errorf("物化间隔分区 %s 失败: %w", want.Name, err)
			}
			actual, e = nativeReadIntervalParts(ctx, conn, w.schema, object.Name)
			if e != nil {
				return e
			}
			for i, got := range actual {
				if intervalHighEqual(got.high, want.HighSQL) {
					match = i
					break
				}
			}
			if match < 0 {
				return fmt.Errorf("间隔种子未生成冻结边界 %s", want.HighSQL)
			}
		}
		got := actual[match]
		if got.name != want.Name {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE "+damengnative.Qualified(w.schema, object.Name)+" RENAME PARTITION "+damengnative.Quote(got.name)+" TO "+damengnative.Quote(want.Name)); err != nil {
				return err
			}
		}
		if want.Tablespace != "" && got.space != want.Tablespace {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE "+damengnative.Qualified(w.schema, object.Name)+" MOVE PARTITION "+damengnative.Quote(want.Name)+" TABLESPACE "+damengnative.Quote(want.Tablespace)); err != nil {
				return err
			}
		}
	}
	if err = w.verifyNativeInterval(ctx, conn, object, false); err != nil {
		return err
	}
	restored, err = nativeIntervalStageRestored(ctx, conn, w.schema, p.EmptyTouch)
	if err != nil {
		return err
	}
	if !restored {
		return fmt.Errorf("间隔分区临时属性未完全恢复")
	}
	return repair.Observe(ctx, unit.SQL, "succeeded", nil)
}

func (w *DaMengNativeWriter) verifyNativeInterval(ctx context.Context, conn *sql.Conn, object damengnative.Object, definitions bool) error {
	p := object.Interval
	if p == nil {
		return nil
	}
	actual, err := nativeReadIntervalParts(ctx, conn, w.schema, object.Name)
	if err != nil {
		return err
	}
	if len(actual) != len(p.Partitions) {
		return fmt.Errorf("目标间隔分区清单数量不同: 源%d 目标%d", len(p.Partitions), len(actual))
	}
	for _, want := range p.Partitions {
		found := false
		for _, got := range actual {
			if got.name != want.Name {
				continue
			}
			found = true
			if !intervalHighEqual(got.high, want.HighSQL) || want.Tablespace != "" && got.space != want.Tablespace {
				return fmt.Errorf("间隔分区 %s 的边界或表空间不同", want.Name)
			}
			if definitions && strings.TrimSpace(want.DefinitionSQL) != "" {
				var ddl string
				if err = conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?)`, got.physical, w.schema).Scan(&ddl); err != nil {
					return err
				}
				ddl, err = nativeRestoreXMLDefinition(ctx, conn, w.schema, got.physical, ddl)
				if err != nil {
					return err
				}
				// Physical partition constraints are derived from the logical
				// parent. Their dictionary-assigned names differ across instances;
				// only a proven native ownership chain permits removing a name.
				rows, e := conn.QueryContext(ctx, `SELECT c.NAME FROM SYS.SYSOBJECTS c JOIN SYS.SYSOBJECTS p ON p.ID=c.PID AND p.SUBTYPE$='UTAB' JOIN SYS.SYSHPARTTABLEINFO h ON h.PART_TABLE_ID=p.ID JOIN SYS.SYSOBJECTS b ON b.ID=h.BASE_TABLE_ID AND b.SUBTYPE$='UTAB' JOIN SYS.SYSOBJECTS s ON s.ID=b.SCHID WHERE c.SUBTYPE$='CONS' AND s.TYPE$='SCH' AND s.NAME=? AND b.NAME=? AND p.NAME=?`, w.schema, object.Name, got.physical)
				if e != nil {
					return e
				}
				var derived []string
				for rows.Next() {
					var name string
					if e = rows.Scan(&name); e != nil {
						break
					}
					derived = append(derived, name)
				}
				iteration := rows.Err()
				_ = rows.Close()
				if e != nil {
					return e
				}
				if iteration != nil {
					return iteration
				}
				if !nativeDefinitionsEqual(nativeIntervalDerivedDefinition(want.DefinitionSQL, want.DerivedConstraintNames), nativeIntervalDerivedDefinition(ddl, derived)) {
					return fmt.Errorf("间隔分区 %s 的原生物理定义不同", want.Name)
				}
			}
		}
		if !found {
			return fmt.Errorf("目标缺少间隔分区 %s", want.Name)
		}
	}
	return nil
}

func nativeIntervalDerivedDefinition(ddl string, names []string) string {
	tokens, err := nativeSQLTokens(ddl)
	if err != nil {
		return ddl
	}
	derived := make(map[string]bool, len(names))
	for _, name := range names {
		derived[name] = true
	}
	var result []string
	for i := 0; i < len(tokens); i++ {
		if strings.EqualFold(tokens[i], "CONSTRAINT") && i+1 < len(tokens) && derived[nativeTokenName(tokens[i+1])] {
			i++
			continue
		}
		result = append(result, tokens[i])
	}
	return strings.Join(result, " ")
}

func (w *DaMengNativeWriter) VerifyDamengNativeIntervalPartitions(ctx context.Context, object damengnative.Object) error {
	if object.Interval == nil {
		return nil
	}
	if object.Schema != w.schema || object.Kind != "table" {
		return fmt.Errorf("间隔分区核验不属于目标表")
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return w.verifyNativeInterval(ctx, conn, object, true)
}

func (w *DaMengNativeWriter) ReconcileDamengNativeIntervalPartitions(ctx context.Context, object damengnative.Object, unit damengnative.Unit) (done, retry bool, err error) {
	if _, err = w.intervalPlan(object, unit); err != nil {
		return false, false, err
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return false, false, err
	}
	defer conn.Close()
	if err = w.verifyNativeInterval(ctx, conn, object, false); err == nil {
		restored, e := nativeIntervalStageRestored(ctx, conn, w.schema, object.Interval.EmptyTouch)
		if e != nil {
			return false, false, e
		}
		if restored {
			return true, false, nil
		}
		err = fmt.Errorf("间隔分区存在但临时表属性尚未恢复")
	}
	// Partial allocation is safe to continue only before any real table data.
	// The original empty-touch implementation reconciles interrupted column,
	// constraint and identity changes before executing another transactional pair.
	var count int64
	if e := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+damengnative.Qualified(w.schema, object.Name)).Scan(&count); e != nil {
		return false, false, e
	}
	if count != 0 {
		return false, false, fmt.Errorf("间隔分区响应未知且目标已有数据，需要整表恢复确认: %w", err)
	}
	return false, true, nil
}

func nativeIntervalTouch(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.IntervalPartitionPlan, part damengnative.IntervalPartition) error {
	touch := *p.EmptyTouch
	touch.Columns = append([]damengnative.MaterializedEmptyColumn{}, p.EmptyTouch.Columns...)
	values := map[string]string{}
	if len(part.Values) == 0 {
		values[p.Column] = part.SeedSQL
	} else {
		for _, value := range part.Values {
			if value.Column == "" || value.SQL == "" || values[value.Column] != "" {
				return fmt.Errorf("间隔分区多个可写列种子不完整或重复")
			}
			values[value.Column] = value.SQL
		}
	}
	for i := range touch.Columns {
		if value, found := values[touch.Columns[i].Name]; found {
			if value == "NULL" {
				if touch.Columns[i].ValueSQL != "" {
					return fmt.Errorf("间隔分区NULL种子不能替换冻结的必需非空值")
				}
				value = ""
			}
			touch.Columns[i].ValueSQL = value
			delete(values, touch.Columns[i].Name)
		}
	}
	if len(values) > 0 {
		return fmt.Errorf("间隔种子不属于目标可写列")
	}
	return nativeHistoryEmptyTouch(ctx, conn, schema, &touch)
}

func nativeIntervalStageRestored(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.MaterializedEmptyTouch) (bool, error) {
	ddl, err := historyEmptyReadDDL(ctx, conn, schema, p.Table)
	if err != nil {
		return false, err
	}
	actual, err := historyEmptyConstraints(ctx, conn, schema, p.Table)
	if err != nil {
		return false, err
	}
	resolved, err := historyResolveEmptyConstraints(actual, p)
	if err != nil {
		return false, err
	}
	for _, want := range resolved {
		for _, got := range actual {
			if got.name == want.name && got.table == want.table && (got.enabled != want.enabled || got.noValidate != want.noValidate) {
				return false, nil
			}
		}
	}
	if len(p.IdentityRestore) > 0 {
		var recipe *nativeHistoryTouchRecipe
		q := damengnative.Qualified(schema, p.Table)
		for _, c := range p.Columns {
			u := "UPDATE " + q + " SET " + damengnative.Quote(c.Name) + "=" + damengnative.Quote(c.Name) + " WHERE ROWID=(SELECT ROWID FROM " + q + " WHERE ROWNUM=1)"
			v, e := parseNativeHistoryTouch(schema, []string{p.IdentityRestore[0], u, p.IdentityRestore[1]})
			if e == nil {
				recipe = &v
				break
			}
		}
		if recipe == nil {
			return false, fmt.Errorf("间隔分区自增恢复定义不可识别")
		}
		present, e := nativeHistoryIdentityPresent(ctx, conn, schema, *recipe)
		if e != nil {
			return false, e
		}
		if !present {
			return false, nil
		}
		if e = nativeHistoryIdentityMatches(ctx, conn, schema, *recipe); e != nil {
			return false, e
		}
	}
	return nativeDefinitionsEqual(p.DefinitionSQL, ddl), nil
}
