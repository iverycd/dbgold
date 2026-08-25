package gaussdb

import (
	"context"
	"database/sql"
	"dbgold/driver/driverconfig"
	"fmt"

	_ "gitee.com/opengauss/openGauss-connector-go-pq"
)

type Driver struct {
	db *sql.DB
}

func New() *Driver { return &Driver{} }

func (d *Driver) Connect(ctx context.Context, opts driverconfig.ConnectOptions) error {
	db, err := sql.Open("opengauss", opts.DSN)
	if err != nil {
		return err
	}
	d.db = db
	return d.Ping(ctx)
}

func (d *Driver) Ping(ctx context.Context) error {
	if d.db == nil {
		return fmt.Errorf("not connected")
	}
	return d.db.PingContext(ctx)
}

func (d *Driver) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}
