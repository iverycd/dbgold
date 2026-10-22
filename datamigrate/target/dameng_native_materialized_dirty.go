package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/damengnative"
)

type nativeHistoryTouchRecipe struct {
	table, column, update, drop, restore string
	seed, step                           int64
}

// The recipe is part of the authenticated native history plan. Its only DML
// is a one-row self assignment; it cannot carry arbitrary SQL from an editor.
func parseNativeHistoryTouch(schema string, statements []string) (nativeHistoryTouchRecipe, error) {
	var p nativeHistoryTouchRecipe
	if len(statements) != 1 && len(statements) != 3 {
		return p, fmt.Errorf("物化状态恢复缺少冻结的单表触碰计划")
	}
	update := statements[len(statements)/2]
	tokens, err := nativeSQLTokens(update)
	if err != nil || len(tokens) < 7 || !strings.EqualFold(tokens[0], "UPDATE") {
		return p, fmt.Errorf("物化状态恢复只能执行冻结的单行原值赋值")
	}
	end, err := nativeHeadIdentity(tokens, 1, schema)
	if err != nil || end+1 >= len(tokens) || !strings.EqualFold(tokens[end], "SET") || !nativeIdentifierToken(tokens[end+1]) {
		return p, fmt.Errorf("物化状态恢复的表或列不属于目标 Schema")
	}
	p.table, p.column = nativeTokenName(tokens[end-1]), nativeTokenName(tokens[end+1])
	q, c := damengnative.Qualified(schema, p.table), damengnative.Quote(p.column)
	expected := "UPDATE " + q + " SET " + c + "=" + c + " WHERE ROWID=(SELECT ROWID FROM " + q + " WHERE ROWNUM=1)"
	if !nativeDefinitionsEqual(update, expected) {
		return p, fmt.Errorf("物化状态恢复不能改变数据值、行定位或对象范围")
	}
	p.update = update
	if len(statements) == 1 {
		return p, nil
	}
	if !nativeDefinitionsEqual(statements[0], "ALTER TABLE "+q+" DROP IDENTITY") {
		return p, fmt.Errorf("物化状态恢复的自增暂停语句与冻结表不符")
	}
	add, err := nativeSQLTokens(statements[2])
	if err != nil || len(add) < 10 || !strings.EqualFold(add[0], "ALTER") || !strings.EqualFold(add[1], "TABLE") {
		return p, fmt.Errorf("物化状态恢复缺少原自增续号定义")
	}
	i, err := nativeHeadIdentity(add, 2, schema)
	if err != nil || nativeTokenName(add[i-1]) != p.table || i+5 >= len(add) || !strings.EqualFold(add[i], "ADD") || !strings.EqualFold(add[i+1], "COLUMN") || nativeTokenName(add[i+2]) != p.column || !strings.EqualFold(add[i+3], "IDENTITY") || add[i+4] != "(" {
		return p, fmt.Errorf("物化状态恢复的自增列身份与冻结计划不符")
	}
	start := i + 5
	comma := start
	for comma < len(add) && add[comma] != "," {
		comma++
	}
	close := comma + 1
	for close < len(add) && add[close] != ")" {
		close++
	}
	if comma >= len(add) || close != len(add)-1 {
		return p, fmt.Errorf("自增续号定义只能包含冻结的种子与步长")
	}
	p.seed, err = strconv.ParseInt(strings.Join(add[start:comma], ""), 10, 64)
	if err != nil {
		return p, fmt.Errorf("冻结的自增续号种子不合法")
	}
	p.step, err = strconv.ParseInt(strings.Join(add[comma+1:close], ""), 10, 64)
	if err != nil || p.step == 0 {
		return p, fmt.Errorf("冻结的自增步长不合法")
	}
	p.drop, p.restore = statements[0], statements[2]
	return p, nil
}

func nativeHistoryIdentityPresent(ctx context.Context, conn *sql.Conn, schema string, p nativeHistoryTouchRecipe) (bool, error) {
	var flags int64
	err := conn.QueryRowContext(ctx, `SELECT c.INFO2 FROM SYS.SYSCOLUMNS c JOIN SYS.SYSOBJECTS o ON o.ID=c.ID JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.NAME=? AND o.NAME=? AND c.NAME=?`, schema, p.table, p.column).Scan(&flags)
	if err != nil {
		return false, fmt.Errorf("无法核对物化状态恢复的自增列身份: %w", err)
	}
	return flags&1 != 0, nil
}

func nativeHistoryIdentityMatches(ctx context.Context, conn *sql.Conn, schema string, p nativeHistoryTouchRecipe) error {
	var seed, step, current int64
	q := damengnative.Qualified(schema, p.table)
	if err := conn.QueryRowContext(ctx, `SELECT IDENT_SEED(?),IDENT_INCR(?),IDENT_CURRENT(?) FROM DUAL`, q, q, q).Scan(&seed, &step, &current); err != nil {
		return fmt.Errorf("无法读取物化状态恢复前的自增续号: %w", err)
	}
	if seed != p.seed || step != p.step || current != p.seed {
		return fmt.Errorf("自增列续号已偏离冻结计划，不能触碰物化状态")
	}
	return nil
}

// nativeHistoryTouch marks the native DML epoch without changing a value or an
// existing physical ROWID. An identity-only table temporarily drops only its
// identity attribute and reinstalls the exact already-frozen next seed/step.
// Unlike INSERT+DELETE this cannot consume a previously unused identity seed,
// violate a uniqueness/FK/check constraint, or need a spare representable ID.
// Business triggers have not been installed at this stage of native recovery.
func nativeHistoryTouch(ctx context.Context, conn *sql.Conn, schema string, statements []string) (retErr error) {
	p, err := parseNativeHistoryTouch(schema, statements)
	if err != nil {
		return err
	}
	if p.restore != "" {
		present, err := nativeHistoryIdentityPresent(ctx, conn, schema, p)
		if err != nil {
			return err
		}
		if present {
			if err := nativeHistoryIdentityMatches(ctx, conn, schema, p); err != nil {
				return err
			}
		}
		// Even a cancelled/unknown DROP must run independent cleanup. A retry
		// first reads the identity flag: an already-restored attribute is only
		// verified, while an interrupted missing attribute is restored once.
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			present, err := nativeHistoryIdentityPresent(cleanup, conn, schema, p)
			if err == nil && !present {
				_, err = conn.ExecContext(cleanup, p.restore)
			}
			if err == nil {
				err = nativeHistoryIdentityMatches(cleanup, conn, schema, p)
			}
			if err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("恢复冻结的自增续号失败，需核对目标目录后补迁: %w", err))
			}
		}()
		if present {
			if _, err := conn.ExecContext(ctx, p.drop); err != nil {
				return err
			}
		}
	}
	result, err := conn.ExecContext(ctx, p.update)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return errors.Join(fmt.Errorf("物化状态恢复必须触碰恰好一个原有基表行，实际 %d", affected), err)
	}
	return nil
}
