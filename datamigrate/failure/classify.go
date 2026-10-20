package failure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"syscall"

	"gitee.com/chunanyong/dm"
	oranetwork "github.com/sijms/go-ora/v2/network"
)

// IsTaskError identifies failures for which continuing with another
// table cannot be trusted. SQL compatibility, object privileges and metadata
// errors remain object failures unless their cause is a lost connection or a
// capability error explicitly scoped to the whole task.
func IsTaskError(err error) bool {
	if err == nil {
		return false
	}
	for _, cause := range []error{
		context.Canceled, context.DeadlineExceeded, driver.ErrBadConn,
		sql.ErrConnDone, net.ErrClosed, io.EOF, io.ErrUnexpectedEOF,
		io.ErrClosedPipe, oranetwork.ErrConnReset,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ENOTCONN,
		syscall.EPIPE, syscall.ETIMEDOUT, syscall.ECONNREFUSED,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH,
	} {
		if errors.Is(err, cause) {
			return true
		}
	}
	var scope interface{ OracleDamengCapabilityScope() string }
	if errors.As(err, &scope) && scope.OracleDamengCapabilityScope() == "task" {
		return true
	}
	var op *net.OpError
	var dns *net.DNSError
	var networkError net.Error
	if errors.As(err, &op) || errors.As(err, &dns) || errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var oracleError *oranetwork.OracleError
	if errors.As(err, &oracleError) && oracleError != nil && oracleTaskConnectionError(oracleError) {
		return true
	}
	var dmError *dm.DmError
	if errors.As(err, &dmError) && dmError != nil && damengTaskConnectionError(dmError) {
		return true
	}
	// errors.As returns the first matching cause. Inspect every joined branch so
	// an ordinary SQL error cannot hide a later scoped or driver connection error.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if IsTaskError(cause) {
				return true
			}
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsTaskError(wrapped.Unwrap())
	}
	return false
}

func oracleTaskConnectionError(err *oranetwork.OracleError) bool {
	// Use the driver's own explicit session-loss list, rather than matching all
	// ORA errors or a message containing words such as "connection".
	if err.Bad() {
		return true
	}
	switch err.ErrCode {
	case 1017, 28000, 28001: // Login credentials/account prevent a new pooled session.
		return true
	case 1035, 1090, 1092, 3136, 6413: // Instance unavailable, terminated or not connected.
		return true
	case 12154, 12170, // Connect identifier resolution / connect timeout.
		12506, 12514, 12516, 12518, // Listener refusal or no usable service/handler.
		12535, 12541, 12543, 12545, 12547, // Network timeout, listener/host/contact loss.
		12560, 12564, 12569, 12570, 12571, 12631: // TNS transport/authentication failure.
		return true
	}
	return false
}

func damengTaskConnectionError(err *dm.DmError) bool {
	// These are connection/protocol errors defined by dm v1.8.23. In particular,
	// ECGO_FATAL_ERROR is excluded: that code also denotes an invalid decimal
	// payload, and is not sufficient evidence that all other tables must stop.
	switch err.ErrCode {
	case dm.ECGO_COMMUNITION_ERROR.ErrCode, dm.ECGO_MSG_CHECK_ERROR.ErrCode,
		dm.ECGO_CONNECTION_CLOSED.ErrCode, dm.ECGO_INVALID_CONN.ErrCode,
		dm.ECGO_UNKOWN_NETWORK.ErrCode, dm.ECGO_CONNECTION_SWITCHED.ErrCode,
		dm.ECGO_CONNECTION_SWITCH_FAILED.ErrCode, dm.ECGO_INIT_SSL_FAILED.ErrCode:
		return true
	}
	return false
}
