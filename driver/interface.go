package driver

import (
	"context"

	"dbgold/driver/driverconfig"
)

// ConnectOptions keeps credentials separate from protocol URLs. Native Go
// drivers use DSN; JDBC-backed drivers also consume Username and Password.
type ConnectOptions = driverconfig.ConnectOptions

type Driver interface {
	Connect(ctx context.Context, opts driverconfig.ConnectOptions) error
	Ping(ctx context.Context) error
	Close() error
}
