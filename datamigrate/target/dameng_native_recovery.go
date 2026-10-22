package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
)

// NativeRecoveryState is read-only. The caller binds these exact statements to
// the data-recovery preview before changing any table state.
func (w *DaMengNativeWriter) NativeRecoveryState(ctx context.Context, table string) (disable, restore []damengnative.Unit, err error) {
	rows, err := w.db.QueryContext(ctx, `SELECT OWNER,TRIGGER_NAME,STATUS FROM ALL_TRIGGERS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY OWNER,TRIGGER_NAME`, w.schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("读取补迁触发器状态失败: %w", err)
	}
	type toggle struct{ disable, restore string }
	var actions []toggle
	for rows.Next() {
		var owner, name, status string
		if err = rows.Scan(&owner, &name, &status); err != nil {
			break
		}
		if owner != w.schema {
			err = fmt.Errorf("目标表存在外部 schema %s 的触发器，不能自动补迁", owner)
			break
		}
		enabled, e := nativeEnabledState(status)
		if e != nil {
			err = e
			break
		}
		if enabled {
			qualified := damengnative.Qualified(owner, name)
			actions = append(actions, toggle{"ALTER TRIGGER " + qualified + " DISABLE", "ALTER TRIGGER " + qualified + " ENABLE"})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = w.db.QueryContext(ctx, `SELECT c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME,c.STATUS,c.VALIDATED FROM ALL_CONSTRAINTS c LEFT JOIN ALL_CONSTRAINTS p ON c.R_OWNER=p.OWNER AND c.R_CONSTRAINT_NAME=p.CONSTRAINT_NAME WHERE c.CONSTRAINT_TYPE='R' AND ((c.OWNER=? AND c.TABLE_NAME=?) OR (p.OWNER=? AND p.TABLE_NAME=?)) ORDER BY c.OWNER,c.TABLE_NAME,c.CONSTRAINT_NAME`, w.schema, table, w.schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("读取补迁外键状态失败: %w", err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var owner, parent, name, status string
		var validated sql.NullString
		if err = rows.Scan(&owner, &parent, &name, &status, &validated); err != nil {
			break
		}
		if owner != w.schema {
			err = fmt.Errorf("存在外部 schema %s.%s 的入向外键 %s，不能清空补迁", owner, parent, name)
			break
		}
		key := owner + "\x00" + parent + "\x00" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		enabled, e := nativeEnabledState(status)
		if e != nil {
			err = e
			break
		}
		if enabled {
			prefix := "ALTER TABLE " + damengnative.Qualified(owner, parent)
			constraint := damengnative.Quote(name)
			enable := " ENABLE CONSTRAINT "
			switch strings.ToUpper(strings.TrimSpace(validated.String)) {
			case "VALIDATED", "Y", "1":
			case "NOT VALIDATED", "NOVALIDATE", "N", "0":
				enable = " ENABLE NOVALIDATE CONSTRAINT "
			default:
				err = fmt.Errorf("外键 %s 的验证状态无法确认", name)
			}
			if err != nil {
				break
			}
			actions = append(actions, toggle{prefix + " DISABLE CONSTRAINT " + constraint, prefix + enable + constraint})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, nil, err
	}
	for _, action := range actions {
		disable = append(disable, damengnative.Unit{SQL: action.disable, Phase: "before_data"})
	}
	for i := len(actions) - 1; i >= 0; i-- {
		restore = append(restore, damengnative.Unit{SQL: actions[i].restore, Phase: "restore"})
	}
	return disable, restore, nil
}

func nativeEnabledState(status string) (bool, error) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "ENABLED", "Y", "1":
		return true, nil
	case "DISABLED", "N", "0":
		return false, nil
	}
	return false, fmt.Errorf("达梦对象启停状态无法确认: %q", status)
}
