package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"gitee.com/chunanyong/dm"
)

func (w *DaMengNativeWriter) ExecuteDamengNativeSensitive(ctx context.Context, display, actual string) error {
	public, err := damengnative.PublicLinkSQL(actual)
	if err != nil || public != display {
		return fmt.Errorf("DBLink 凭据执行单元与冻结公开定义不符")
	}
	return w.executeDamengNativeStatement(ctx, actual, display, true)
}

// A DM error may echo the submitted password. Keep only its numeric code and
// the uncertainty class; never wrap the original server error in durable state.
func nativeLinkError(err error) error {
	if err == nil {
		return nil
	}
	message := "DBLink 执行失败（服务端详情已脱敏）"
	var dmErr *dm.DmError
	communication := false
	if errors.As(err, &dmErr) {
		message = fmt.Sprintf("DBLink 执行失败（达梦错误码 %d，详情已脱敏）", dmErr.ErrCode)
		communication = dmErr.ErrCode == dm.ECGO_COMMUNITION_ERROR.ErrCode || dmErr.ErrCode == dm.ECGO_CONNECTION_CLOSED.ErrCode
	}
	if communication || uncertainRepairError(err) {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			if errors.Is(err, cause) {
				return fmt.Errorf("%s: %w", message, cause)
			}
		}
		return fmt.Errorf("%s；执行结果未知: %w", message, driver.ErrBadConn)
	}
	return errors.New(message)
}

func execNativeLinkObserved(ctx context.Context, executor sqlExecutor, display, actual string) error {
	if err := repair.Observe(ctx, display, "running", nil); err != nil {
		return fmt.Errorf("保存 DBLink 执行前状态: %w", err)
	}
	_, err := executor.ExecContext(ctx, actual)
	err = nativeLinkError(err)
	status := "succeeded"
	if err != nil {
		status = repairErrorStatus(err)
	}
	return errors.Join(err, repair.Observe(ctx, display, status, err))
}
