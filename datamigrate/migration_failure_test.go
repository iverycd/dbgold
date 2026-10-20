package datamigrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"gitee.com/chunanyong/dm"
	oranetwork "github.com/sijms/go-ora/v2/network"
	"github.com/stretchr/testify/require"
)

type migrationScopedFailure struct {
	scope string
	cause error
}

func (e migrationScopedFailure) Error() string                       { return "capability check" }
func (e migrationScopedFailure) Unwrap() error                       { return e.cause }
func (e migrationScopedFailure) OracleDamengCapabilityScope() string { return e.scope }

type migrationNetworkTimeout struct{}

func (migrationNetworkTimeout) Error() string   { return "operation timed out" }
func (migrationNetworkTimeout) Timeout() bool   { return true }
func (migrationNetworkTimeout) Temporary() bool { return true }

func TestMigrationTaskFailureCauses(t *testing.T) {
	for _, cause := range []error{
		context.Canceled, context.DeadlineExceeded, driver.ErrBadConn,
		sql.ErrConnDone, net.ErrClosed, io.EOF, io.ErrUnexpectedEOF,
		io.ErrClosedPipe, oranetwork.ErrConnReset,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ENOTCONN,
		syscall.EPIPE, syscall.ETIMEDOUT, syscall.ECONNREFUSED,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("transport failed")},
		&net.DNSError{Err: "no such host", Name: "db.invalid"},
		&os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET},
		migrationNetworkTimeout{},
		migrationScopedFailure{scope: "task", cause: errors.New("literal binding changes data")},
		migrationScopedFailure{scope: "table", cause: driver.ErrBadConn},
	} {
		t.Run(fmt.Sprintf("%T_%s", cause, cause.Error()), func(t *testing.T) {
			require.True(t, isMigrationTaskError(cause))
			require.True(t, isMigrationTaskError(fmt.Errorf("源表 T: %w", fmt.Errorf("读取元数据: %w", cause))))
		})
	}
}

func TestMigrationOracleConnectionCodes(t *testing.T) {
	for _, code := range []int{
		28, 1012, 1033, 1034, 1089, 3113, 3114, 3135, 12528, 12537,
		1017, 28000, 28001, 1035, 1090, 1092, 3136, 6413,
		12154, 12170, 12506, 12514, 12516, 12518, 12535, 12541,
		12543, 12545, 12547, 12560, 12564, 12569, 12570, 12571, 12631,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			err := fmt.Errorf("读取源表 T: %w", &oranetwork.OracleError{ErrCode: code, ErrMsg: "driver error"})
			require.True(t, isMigrationTaskError(err))
		})
	}
}

func TestMigrationDamengConnectionCodes(t *testing.T) {
	for _, code := range []int32{6001, 6002, 6060, 9007, 9008, 20000, 20001, 20002} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			err := fmt.Errorf("目标能力检查: %w", &dm.DmError{ErrCode: code, ErrText: "driver error"})
			require.True(t, isMigrationTaskError(err))
		})
	}
}

func TestMigrationObjectFailuresDoNotStopHealthyTables(t *testing.T) {
	require.False(t, isMigrationTaskError(nil))
	for _, cause := range []error{
		sql.ErrNoRows, sql.ErrTxDone,
		errors.New("column T.C cannot be converted"),
		errors.New("connection reset in unsupported default literal"),
		errors.New("driver: bad connection"),
		errors.New("ORA-03135: text inside object definition"),
		errors.New("EOF"),
		&net.AddrError{Err: "invalid datatype address", Addr: "column"},
		migrationScopedFailure{scope: "table", cause: errors.New("national type unavailable")},
		migrationScopedFailure{scope: "task_typo", cause: errors.New("ordinary error")},
		&oranetwork.OracleError{ErrCode: 904, ErrMsg: "invalid identifier"},
		&oranetwork.OracleError{ErrCode: 942, ErrMsg: "table or view does not exist"},
		&oranetwork.OracleError{ErrCode: 1031, ErrMsg: "insufficient privileges"},
		&oranetwork.OracleError{ErrCode: 1013, ErrMsg: "current SQL canceled; no context cancellation"},
		&oranetwork.OracleError{ErrCode: 6502, ErrMsg: "value conversion error"},
		&dm.DmError{ErrCode: -5515, ErrText: "无权限"},
		&dm.DmError{ErrCode: -2106, ErrText: "无效的表或视图名"},
		&dm.DmError{ErrCode: -6111, ErrText: "字符串转换出错"},
		&dm.DmError{ErrCode: 6007, ErrText: "data conversion error"},
		&dm.DmError{ErrCode: 6034, ErrText: "resultset closed"},
		&dm.DmError{ErrCode: 6035, ErrText: "statement closed"},
		&dm.DmError{ErrCode: 20004, ErrText: "invalid decimal payload"},
	} {
		t.Run(fmt.Sprintf("%T_%s", cause, cause.Error()), func(t *testing.T) {
			require.False(t, isMigrationTaskError(cause))
			require.False(t, isMigrationTaskError(fmt.Errorf("表 T 预检失败: %w", cause)))
		})
	}
}

func TestMigrationTaskFailureFindsLaterJoinedCause(t *testing.T) {
	for _, err := range []error{
		errors.Join(errors.New("ordinary table error"), context.Canceled),
		errors.Join(&oranetwork.OracleError{ErrCode: 904}, &oranetwork.OracleError{ErrCode: 3135}),
		errors.Join(&dm.DmError{ErrCode: -5515}, &dm.DmError{ErrCode: 6001}),
		errors.Join(migrationScopedFailure{scope: "table"}, migrationScopedFailure{scope: "task"}),
	} {
		require.True(t, isMigrationTaskError(fmt.Errorf("预检: %w", err)))
	}
	require.False(t, isMigrationTaskError(errors.Join(&oranetwork.OracleError{ErrCode: 904}, &dm.DmError{ErrCode: -5515})))
}
