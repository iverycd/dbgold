package oscar

import (
	"context"
	"fmt"
	"time"

	"dbgold/driver/driverconfig"
	"dbgold/internal/jdbcbridge"
)

type Driver struct {
	bridge  *jdbcbridge.Manager
	session uint64
}

func New() *Driver { return &Driver{bridge: jdbcbridge.Default()} }

func (d *Driver) Connect(ctx context.Context, opts driverconfig.ConnectOptions) error {
	if d.session != 0 {
		_ = d.Close()
	}
	session, err := d.bridge.OpenSession(ctx, jdbcbridge.SessionOptions{
		URL: opts.DSN, Username: opts.Username, Password: opts.Password,
		MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		return err
	}
	d.session = session
	return nil
}

func (d *Driver) Ping(ctx context.Context) error {
	if d.session == 0 {
		return fmt.Errorf("not connected")
	}
	return d.bridge.Ping(ctx, d.session)
}

func (d *Driver) ListSchemas(ctx context.Context) ([]string, error) {
	if d.session == 0 {
		return nil, fmt.Errorf("not connected")
	}
	return d.bridge.ListSchemas(ctx, d.session)
}

func (d *Driver) Close() error {
	if d.session == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := d.bridge.CloseSession(ctx, d.session)
	d.session = 0
	return err
}
