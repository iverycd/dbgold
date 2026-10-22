package target

import (
	"context"
	"database/sql"
	"fmt"

	"dbgold/datamigrate/damengnative"
)

// A protected HASH-partitioned MV table cannot be the operand of partition
// exchange. DBMS_REDEFINITION can populate it as an interim table instead. The
// engine retains the original table's physical identity, indexes, partitioning,
// and the MV's refresh watermark; FINISH exchanges only the two table names.
// Reversing that name exchange returns the original storage to its exact name.
// ROWID mode works for keyless tables and duplicate rows, and FINISH removes its
// temporary RID$$ column. All input rows come from a separate immutable staging
// table; the work table is deliberately disposable.
func historyRebaseStorage(ctx context.Context, conn *sql.Conn, schema string, v damengnative.MaterializedHistoryView) error {
	if err := historyRedefinitionFields(v); err != nil {
		return err
	}
	before, err := historyReadRedefinitionState(ctx, conn, schema, v)
	if err != nil {
		return err
	}
	if before.boundName != v.Table || before.names[v.Redefinition.WorkingTable] == 0 || before.names[v.Redefinition.SwapName] != 0 {
		return fmt.Errorf("物化重定义必须先核对并恢复冻结存储名称")
	}
	var cutoff string
	if err = historyRedefinitionCutoff(ctx, conn, schema, v.Name, &cutoff); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `CALL SYS.DBMS_REDEFINITION.START_REDEF_TABLE(?,?,?,NULL,2)`, schema, v.Redefinition.WorkingTable, v.Table); err != nil {
		return fmt.Errorf("物化视图 %s 向原始分区存储导入冻结快照失败: %w", v.Name, err)
	}
	if _, err = conn.ExecContext(ctx, `CALL SYS.DBMS_REDEFINITION.FINISH_REDEF_TABLE(?,?,?)`, schema, v.Redefinition.WorkingTable, v.Table); err != nil {
		return fmt.Errorf("物化视图 %s 完成原始分区存储导入失败: %w", v.Name, err)
	}
	if err = historyNormalizeRedefinitionNames(ctx, conn, schema, v); err != nil {
		return err
	}
	after, err := historyReadRedefinitionState(ctx, conn, schema, v)
	if err != nil {
		return err
	}
	var afterCutoff string
	if err = historyRedefinitionCutoff(ctx, conn, schema, v.Name, &afterCutoff); err != nil {
		return err
	}
	if after.boundID != before.boundID || after.boundName != v.Table || cutoff != afterCutoff {
		return fmt.Errorf("物化视图 %s 原存储身份或原生刷新水位在重定义时改变", v.Name)
	}
	return nil
}

// This must run before COMPLETE establishes a new group baseline or the work
// table is refilled. A lost FINISH/RENAME response is resolved through the MV's
// actual bound table ID, never by replaying an assumed name swap. An unfinished
// START is aborted using its catalog-proved dependency on both frozen tables.
func historyReconcileRedefinition(ctx context.Context, conn *sql.Conn, schema string, v damengnative.MaterializedHistoryView) error {
	if v.Redefinition == nil {
		return nil
	}
	if err := historyRedefinitionFields(v); err != nil {
		return err
	}
	if err := historyNormalizeRedefinitionNames(ctx, conn, schema, v); err != nil {
		return err
	}
	generated := "MVIEW$_" + v.Table
	var present int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND TYPE$='SCHOBJ' AND NAME=?`, schema, generated).Scan(&present); err != nil {
		return err
	}
	if present != 0 {
		var dependencies int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(DISTINCT REFERENCED_NAME) FROM ALL_DEPENDENCIES WHERE OWNER=? AND NAME=? AND TYPE='MATERIALIZED VIEW' AND REFERENCED_OWNER=? AND REFERENCED_NAME IN (?,?)`, schema, generated, schema, v.Table, v.Redefinition.WorkingTable).Scan(&dependencies); err != nil {
			return err
		}
		if dependencies != 2 {
			return fmt.Errorf("物化重定义临时对象 %s 的依赖不属于冻结工作表与原存储", generated)
		}
		if _, err := conn.ExecContext(ctx, `CALL SYS.DBMS_REDEFINITION.ABORT_REDEF_TABLE(?,?,?)`, schema, v.Redefinition.WorkingTable, v.Table); err != nil {
			return fmt.Errorf("物化视图 %s 清理未完成的原生重定义失败: %w", v.Name, err)
		}
	}
	// ABORT removes RID$$ and the generated MV/log/trigger. No arbitrary DROP is
	// used, so an unexpected catalog object can never be silently deleted.
	var remaining int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND NAME IN (?,?,?)`, schema, generated, "MLOG$_"+v.Redefinition.WorkingTable, "MTRG$_"+v.Redefinition.WorkingTable).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("物化视图 %s 原生重定义临时对象尚未清理", v.Name)
	}
	return nil
}

type historyRedefinitionState struct {
	boundID   int64
	boundName string
	names     map[string]int64
}

func historyReadRedefinitionState(ctx context.Context, conn *sql.Conn, schema string, v damengnative.MaterializedHistoryView) (historyRedefinitionState, error) {
	s := historyRedefinitionState{names: map[string]int64{}}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	// INFO3's low 32 bits are the engine's prebuilt-table binding. Joining the
	// catalog resolves the current physical table name even after FINISH changed
	// it; stored GET_DDL text alone still contains the original name at that point.
	err := conn.QueryRowContext(ctx, `SELECT T.ID,T.NAME FROM SYS.SYSOBJECTS V JOIN SYS.SYSOBJECTS T ON T.ID=BITAND(V.INFO3,4294967295) WHERE V.SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND V.NAME=? AND V.TYPE$='SCHOBJ' AND T.SCHID=V.SCHID AND T.SUBTYPE$='UTAB'`, schema, v.Name).Scan(&s.boundID, &s.boundName)
	if err != nil {
		return s, fmt.Errorf("无法核对物化视图 %s 绑定的物理存储: %w", v.Name, err)
	}
	r := v.Redefinition
	if s.boundName != v.Table && s.boundName != r.WorkingTable && s.boundName != r.SwapName {
		return s, fmt.Errorf("物化视图 %s 绑定的存储 %s 不在冻结重定义名称中", v.Name, s.boundName)
	}
	rows, err := conn.QueryContext(ctx, `SELECT NAME,ID FROM SYS.SYSOBJECTS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND TYPE$='SCHOBJ' AND SUBTYPE$='UTAB' AND NAME IN (?,?,?)`, schema, v.Table, r.WorkingTable, r.SwapName)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return s, err
		}
		s.names[name] = id
	}
	if err = rows.Err(); err != nil {
		return s, err
	}
	if len(s.names) != 2 || s.names[s.boundName] != s.boundID {
		return s, fmt.Errorf("物化视图 %s 冻结重定义的两个物理表与名称不一致", v.Name)
	}
	return s, nil
}

func historyNormalizeRedefinitionNames(ctx context.Context, conn *sql.Conn, schema string, v damengnative.MaterializedHistoryView) error {
	for attempt := 0; attempt < 4; attempt++ {
		s, err := historyReadRedefinitionState(ctx, conn, schema, v)
		if err != nil {
			return err
		}
		r := v.Redefinition
		var from, to string
		switch {
		case s.boundName == v.Table && s.names[r.WorkingTable] != 0 && s.names[r.SwapName] == 0:
			return nil
		case s.boundName == r.WorkingTable && s.names[v.Table] != 0 && s.names[r.SwapName] == 0:
			from, to = v.Table, r.SwapName
		case s.boundName == r.WorkingTable && s.names[v.Table] == 0 && s.names[r.SwapName] != 0:
			from, to = r.WorkingTable, v.Table
		case s.boundName == v.Table && s.names[r.WorkingTable] == 0 && s.names[r.SwapName] != 0:
			from, to = r.SwapName, r.WorkingTable
		default:
			return fmt.Errorf("物化视图 %s 重定义名称状态不属于冻结执行的任一步骤", v.Name)
		}
		if _, err = conn.ExecContext(ctx, "ALTER TABLE "+damengnative.Qualified(schema, from)+" RENAME TO "+damengnative.Quote(to)); err != nil {
			return fmt.Errorf("物化视图 %s 恢复原存储名称失败: %w", v.Name, err)
		}
	}
	return fmt.Errorf("物化视图 %s 重定义名称恢复未完成", v.Name)
}

func historyRedefinitionCutoff(ctx context.Context, conn *sql.Conn, schema, name string, dest *string) error {
	return conn.QueryRowContext(ctx, `SELECT TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, schema, name).Scan(dest)
}

func historyRedefinitionFields(v damengnative.MaterializedHistoryView) error {
	r := v.Redefinition
	if r == nil || v.Table == "" || v.Name == "" || r.WorkingTable == "" || r.SwapName == "" || r.WorkingTable == r.SwapName || r.WorkingTable == v.Table || r.SwapName == v.Table || r.WorkingTable == v.Staging || r.SwapName == v.Staging {
		return fmt.Errorf("物化重定义的原存储、不可变快照与临时表名称不完整或重叠")
	}
	return nil
}

// The preview and startup preflight share this strictly read-only check. The
// work table itself is a regular frozen before_data object; these additional
// names are owned transiently by the documented DBMS_REDEFINITION package.
func (w *DaMengNativeWriter) validateHistoryRedefinition(ctx context.Context, views []damengnative.MaterializedHistoryView, planned ...[]damengnative.Object) error {
	for _, objects := range planned {
		if err := historyRedefinitionPlanConflicts(w.schema, views, objects); err != nil {
			return err
		}
	}
	checked := false
	for _, v := range views {
		if v.Redefinition == nil {
			continue
		}
		if err := historyRedefinitionFields(v); err != nil {
			return err
		}
		if !checked {
			var valid, allowed int
			if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER='SYS' AND OBJECT_NAME='DBMS_REDEFINITION' AND OBJECT_TYPE IN ('PACKAGE','PACKAGE BODY') AND STATUS='VALID'`).Scan(&valid); err != nil {
				return err
			}
			if valid != 2 {
				return fmt.Errorf("目标缺少有效的 SYS.DBMS_REDEFINITION 包及包体")
			}
			if err := w.db.QueryRowContext(ctx, `SELECT CASE WHEN USER IN ('SYS','SYSDBA') OR EXISTS(SELECT 1 FROM SESSION_PRIVS WHERE PRIVILEGE='EXECUTE ANY PACKAGE') OR EXISTS(SELECT 1 FROM ALL_TAB_PRIVS WHERE OWNER='SYS' AND TABLE_NAME='DBMS_REDEFINITION' AND PRIVILEGE='EXECUTE' AND (GRANTEE=USER OR GRANTEE='PUBLIC' OR GRANTEE IN (SELECT ROLE FROM SESSION_ROLES))) THEN 1 ELSE 0 END FROM DUAL`).Scan(&allowed); err != nil {
				return err
			}
			if allowed != 1 {
				return fmt.Errorf("目标执行用户缺少 SYS.DBMS_REDEFINITION 的 EXECUTE 权限")
			}
			checked = true
		}
		var conflicts int
		if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME IN (?,?,?,?)`, w.schema, v.Redefinition.SwapName, "MVIEW$_"+v.Table, "MLOG$_"+v.Redefinition.WorkingTable, "MTRG$_"+v.Redefinition.WorkingTable).Scan(&conflicts); err != nil {
			return err
		}
		if conflicts != 0 {
			return fmt.Errorf("物化视图 %s 重定义临时名称与目标已有对象冲突", v.Name)
		}
	}
	return nil
}

// Reserving just existing target objects is insufficient: a source routine or
// table with one of the package's generated names may otherwise be overwritten
// later in this same migration. Compare exact frozen names across the complete
// object plan before any target SQL is executed.
func historyRedefinitionPlanConflicts(schema string, views []damengnative.MaterializedHistoryView, objects []damengnative.Object) error {
	reserved := map[string]string{}
	for _, v := range views {
		if v.Redefinition == nil {
			continue
		}
		if err := historyRedefinitionFields(v); err != nil {
			return err
		}
		for _, name := range []string{v.Redefinition.SwapName, "MVIEW$_" + v.Table, "MLOG$_" + v.Redefinition.WorkingTable, "MTRG$_" + v.Redefinition.WorkingTable} {
			if previous, exists := reserved[name]; exists {
				return fmt.Errorf("物化视图 %s 与 %s 的重定义临时名称 %s 重叠", previous, v.Name, name)
			}
			reserved[name] = v.Name
		}
	}
	for _, o := range objects {
		if o.Schema != schema {
			continue
		}
		if view, exists := reserved[o.Name]; exists {
			return fmt.Errorf("物化视图 %s 重定义临时名称 %s 与本次计划对象 %s 冲突", view, o.Name, o.Kind)
		}
	}
	return nil
}
