package target

import (
	"context"
	"fmt"

	"dbgold/datamigrate/damengnative"
)

// An unbuilt FAST/FORCE snapshot has no initialized native refresh watermark.
// Row counts and CREATE DDL alone cannot prove that BUILD DEFERRED survived:
// a needless COMPLETE of an empty base could otherwise pass both checks.
func (w *DaMengNativeWriter) verifyNativeDeferredMaterialized(ctx context.Context, object damengnative.Object) error {
	state := object.Materialized
	const uninitialized = "1900-01-01 00:00:00.000000"
	if state == nil || state.Staleness != "UNUSEABLE" || state.LastRefresh == "" || state.LastRefresh > uninitialized {
		return nil
	}
	var actual, watermark string
	if err := w.db.QueryRowContext(ctx, `SELECT STALENESS,TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, w.schema, object.Name).Scan(&actual, &watermark); err != nil {
		return fmt.Errorf("未构建物化视图 %s 的原生状态不可核验: %w", object.Name, err)
	}
	if actual != "UNUSEABLE" || watermark != state.LastRefresh {
		return fmt.Errorf("未构建物化视图 %s 的原生状态或未初始化水位与源端不同", object.Name)
	}
	return nil
}
