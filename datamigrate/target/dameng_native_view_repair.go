package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
)

// A materialized view can also be labelled VIEW in ALL_OBJECTS on DM8.
func (w *DaMengNativeWriter) editedViewState(ctx context.Context, name string) (ddl, status string, err error) {
	var kind string
	err = w.db.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS (
		SELECT 1 FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?
	) THEN 'MATERIALIZED VIEW' ELSE OBJECT_TYPE END, STATUS
	FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE IN ('VIEW','MATERIALIZED VIEW','TABLE','SYNONYM')`,
		w.schema, name, w.schema, name).Scan(&kind, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if kind != "VIEW" {
		return "", "", fmt.Errorf("目标同名对象 %s 类型为 %s", name, kind)
	}
	err = w.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('VIEW',?,?) FROM DUAL`, name, w.schema).Scan(&ddl)
	if err == nil && strings.TrimSpace(ddl) == "" {
		err = fmt.Errorf("无法读取目标视图 %s 的实际定义", name)
	}
	return ddl, status, err
}

func sameNativeViewDefinition(schema, name, expected, actual string) bool {
	a, err := damengnative.ViewRepairDefinition(damengnative.StripClientDelimiter(expected), schema, name)
	if err != nil {
		return false
	}
	b, err := damengnative.ViewRepairDefinition(damengnative.StripClientDelimiter(actual), schema, name)
	return err == nil && nativeDefinitionsEqual(a, b)
}

func (w *DaMengNativeWriter) checkEditedNativeView(ctx context.Context, obj repair.Object, i int) (done, retry bool, err error) {
	ddl, status, err := w.editedViewState(ctx, obj.TargetName)
	if err != nil {
		return false, false, err
	}
	if ddl == "" {
		if i == 0 {
			return false, true, nil
		}
		return false, false, fmt.Errorf("待编译视图 %s 不存在", obj.TargetName)
	}
	if sameNativeViewDefinition(w.schema, obj.TargetName, obj.Native.Units[0].SQL, ddl) {
		if i == 0 || strings.EqualFold(status, "VALID") {
			return true, false, nil
		}
		return false, true, nil
	}
	if i == 0 && obj.NativeViewDraft.InstalledSQL != "" && sameNativeViewDefinition(w.schema, obj.TargetName, obj.NativeViewDraft.InstalledSQL, ddl) {
		return false, true, nil
	}
	return false, false, fmt.Errorf("目标同名视图 %s 的定义与本次修订及已提交定义不一致", obj.TargetName)
}

func (w *DaMengNativeWriter) executeEditedNativeViewUnit(ctx context.Context, obj repair.Object, i int) error {
	done, _, err := w.checkEditedNativeView(ctx, obj, i)
	if err != nil || i == 0 && done {
		return err
	}
	if err = damengnative.ExecuteUnit(ctx, w, *obj.Native, obj.Native.Units[i]); err != nil {
		return err
	}
	if i == 1 {
		valid, _, err := w.checkEditedNativeView(ctx, obj, i)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("达梦视图 %s 编译后仍未达到 VALID 状态", obj.TargetName)
		}
	}
	return nil
}
