package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"dbgold/datamigrate/damengnative"
)

func nativeHistoryGeneratedDefinition(p *damengnative.MaterializedEmptyTouch) (string, []string, error) {
	if p == nil || p.DefinitionSQL == "" || len(p.GeneratedColumns) == 0 {
		return "", nil, fmt.Errorf("缺少冻结生成分区键定义")
	}
	writable := map[string]bool{}
	for _, c := range p.Columns {
		writable[c.Name] = true
	}
	seen := map[string]bool{}
	ddl := p.DefinitionSQL
	var original []string
	for _, c := range p.GeneratedColumns {
		if c.Name == "" || c.ValueSQL == "" || writable[c.Name] || seen[c.Name] {
			return "", nil, fmt.Errorf("生成分区键临时常量不属于冻结非可写列")
		}
		seen[c.Name] = true
		declaration, err := damengnative.NativeGeneratedColumnDeclaration(p.DefinitionSQL, c.Name)
		if err != nil {
			return "", nil, err
		}
		original = append(original, declaration)
		ddl, err = damengnative.RewriteNativeGeneratedColumnConstant(ddl, c.Name, c.ValueSQL)
		if err != nil {
			return "", nil, err
		}
	}
	return ddl, original, nil
}

// A process may stop between modifying two generated keys. Validate each
// declaration against its two frozen possibilities before normalizing it back
// to the original; no unrecognized expression is ever overwritten by recovery.
func nativeHistoryGeneratedCurrent(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.MaterializedEmptyTouch, temporary string) error {
	actual, err := historyEmptyReadDDL(ctx, conn, schema, p.Table)
	if err != nil {
		return err
	}
	for _, c := range p.GeneratedColumns {
		got, e := damengnative.NativeGeneratedColumnDeclaration(actual, c.Name)
		if e != nil {
			return e
		}
		original, e := damengnative.NativeGeneratedColumnDeclaration(p.DefinitionSQL, c.Name)
		if e != nil {
			return e
		}
		constant, e := damengnative.NativeGeneratedColumnDeclaration(temporary, c.Name)
		if e != nil {
			return e
		}
		if !nativeDefinitionsEqual(got, original) && !nativeDefinitionsEqual(got, constant) {
			return fmt.Errorf("空基表生成分区键 %s 已偏离冻结表达式", c.Name)
		}
		actual, e = damengnative.RestoreNativeGeneratedColumnDeclaration(actual, c.Name, p.DefinitionSQL)
		if e != nil {
			return e
		}
	}
	want, err := historyEmptyDefinition(p.DefinitionSQL, p)
	if err != nil {
		return err
	}
	got, err := historyEmptyDefinition(actual, p)
	if err != nil {
		return err
	}
	if !nativeDefinitionsEqual(want, got) {
		return fmt.Errorf("空基表生成键的表定义已偏离冻结计划")
	}
	constraints, err := historyEmptyConstraints(ctx, conn, schema, p.Table)
	if err != nil {
		return err
	}
	_, err = historyResolveEmptyConstraints(constraints, p)
	return err
}

// The empty epoch must use an already existing partition even when evaluating
// the original expression on NULL inputs would route elsewhere. Replace only
// the generated routing expressions, retain them as generated columns, and
// delegate all writable-column/constraint/transaction handling to the existing
// empty-touch executor. The source expression is never evaluated by preflight.
func nativeHistoryGeneratedEmptyTouch(ctx context.Context, conn *sql.Conn, schema string, p *damengnative.MaterializedEmptyTouch) (retErr error) {
	temporary, original, err := nativeHistoryGeneratedDefinition(p)
	if err != nil {
		return err
	}
	q := damengnative.Qualified(schema, p.Table)
	var count int64
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("空基表已有数据，拒绝修改生成分区键")
	}
	if err = nativeHistoryGeneratedCurrent(ctx, conn, schema, p, temporary); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		var n int64
		e := conn.QueryRowContext(cleanup, "SELECT COUNT(*) FROM "+q).Scan(&n)
		if e == nil && n != 0 {
			e = fmt.Errorf("空基表仍有临时数据，拒绝生成键清理DDL")
		}
		if e == nil {
			for i := len(original) - 1; i >= 0; i-- {
				_, restoreErr := conn.ExecContext(cleanup, "ALTER TABLE "+q+" MODIFY "+original[i])
				e = errors.Join(e, restoreErr)
			}
		}
		if e == nil {
			var ddl string
			ddl, e = historyEmptyReadDDL(cleanup, conn, schema, p.Table)
			if e == nil {
				want, wantErr := historyEmptyDefinition(p.DefinitionSQL, p)
				got, gotErr := historyEmptyDefinition(ddl, p)
				e = errors.Join(wantErr, gotErr)
				if e == nil && !nativeDefinitionsEqual(want, got) {
					e = fmt.Errorf("空基表生成分区键声明未完整恢复")
				}
			}
		}
		if e != nil {
			retErr = errors.Join(retErr, fmt.Errorf("恢复空基表生成键失败，需目录核对后补迁: %w", e))
		}
	}()
	for _, c := range p.GeneratedColumns {
		declaration, e := damengnative.NativeGeneratedColumnDeclaration(temporary, c.Name)
		if e != nil {
			return e
		}
		if _, e = conn.ExecContext(ctx, "ALTER TABLE "+q+" MODIFY "+declaration); e != nil {
			return e
		}
	}
	local := *p
	local.DefinitionSQL = temporary
	local.GeneratedColumns = nil
	return nativeHistoryEmptyTouch(ctx, conn, schema, &local)
}
