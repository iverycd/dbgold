package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"dbgold/datamigrate/damengnative"
)

// Reconciliation admits only the original generation expression or one of the
// native constants authenticated by this plan. All other parts of the table
// must still match, except the exact temporary attributes already handled by
// the empty-touch recovery. This function never writes.
func nativeIntervalGeneratedCurrent(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.IntervalPartitionPlan) (string, error) {
	actual, err := historyEmptyReadDDL(ctx, conn, schema, p.EmptyTouch.Table)
	if err != nil {
		return "", err
	}
	got, err := historyEmptyDefinition(actual, p.EmptyTouch)
	if err != nil {
		return "", err
	}
	var candidates = []string{p.EmptyTouch.DefinitionSQL}
	for _, part := range p.Partitions {
		if part.SeedSQL == "" {
			continue
		}
		candidate, e := damengnative.RewriteNativeGeneratedColumnConstant(p.EmptyTouch.DefinitionSQL, p.Column, part.SeedSQL)
		if e != nil {
			return "", e
		}
		candidates = append(candidates, candidate)
	}
	for _, candidate := range candidates {
		want, e := historyEmptyDefinition(candidate, p.EmptyTouch)
		if e != nil {
			return "", e
		}
		if nativeDefinitionsEqual(want, got) {
			constraints, e := historyEmptyConstraints(ctx, conn, schema, p.EmptyTouch.Table)
			if e == nil {
				_, e = historyResolveEmptyConstraints(constraints, p.EmptyTouch)
			}
			return actual, e
		}
	}
	return "", fmt.Errorf("间隔生成键表达式或表定义已偏离冻结计划")
}

// On an empty target DM permits a native MODIFY of a generated partition
// column. Allocating an interval using a constant avoids solving arbitrary
// generation expressions or invoking them on the source. The generated column
// remains generated throughout: it is never a writable INSERT column.
func nativeIntervalGeneratedTouch(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.IntervalPartitionPlan, part damengnative.IntervalPartition) (retErr error) {
	if p == nil || p.EmptyTouch == nil || !p.GeneratedKey || part.SeedSQL == "" || len(part.Values) != 0 {
		return fmt.Errorf("缺少冻结的间隔生成键常量计划")
	}
	q := damengnative.Qualified(schema, p.EmptyTouch.Table)
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("目标表已有数据，拒绝临时修改间隔生成键")
	}
	if _, err := nativeIntervalGeneratedCurrent(ctx, conn, schema, p); err != nil {
		return err
	}
	temporary, err := damengnative.RewriteNativeGeneratedColumnConstant(p.EmptyTouch.DefinitionSQL, p.Column, part.SeedSQL)
	if err != nil {
		return err
	}
	declaration, err := damengnative.NativeGeneratedColumnDeclaration(temporary, p.Column)
	if err != nil {
		return err
	}
	original, err := damengnative.NativeGeneratedColumnDeclaration(p.EmptyTouch.DefinitionSQL, p.Column)
	if err != nil {
		return err
	}
	// MODIFY can succeed before its response is lost. Always attempt cleanup
	// after sending it, even when the request itself returns an error. If the
	// transactional touch left data, refuse DDL rather than implicitly commit it.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		var n int64
		e := conn.QueryRowContext(cleanup, "SELECT COUNT(*) FROM "+q).Scan(&n)
		if e == nil && n != 0 {
			e = fmt.Errorf("间隔生成键清理前仍存在数据，拒绝执行隐式提交DDL")
		}
		if e == nil {
			_, e = conn.ExecContext(cleanup, "ALTER TABLE "+q+" MODIFY "+original)
		}
		if e == nil {
			var ddl string
			ddl, e = historyEmptyReadDDL(cleanup, conn, schema, p.EmptyTouch.Table)
			if e == nil {
				var want, got string
				want, e = historyEmptyDefinition(p.EmptyTouch.DefinitionSQL, p.EmptyTouch)
				if e == nil {
					got, e = historyEmptyDefinition(ddl, p.EmptyTouch)
				}
				if e == nil && !nativeDefinitionsEqual(want, got) {
					e = fmt.Errorf("间隔生成键的原生声明未完整恢复")
				}
			}
		}
		if e != nil {
			retErr = errors.Join(retErr, fmt.Errorf("恢复冻结间隔生成键失败，需目录核对后补迁: %w", e))
		}
	}()
	if _, err = conn.ExecContext(ctx, "ALTER TABLE "+q+" MODIFY "+declaration); err != nil {
		return err
	}
	touch := *p.EmptyTouch
	touch.DefinitionSQL = temporary
	touch.Columns = append([]damengnative.MaterializedEmptyColumn{}, p.EmptyTouch.Columns...)
	return nativeHistoryEmptyTouch(ctx, conn, schema, &touch)
}
