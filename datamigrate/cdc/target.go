package cdc

import (
	"context"
	"database/sql"

	_ "dbgold/internal/highgopq"
	_ "gitee.com/chunanyong/dm"
	_ "gitee.com/opengauss/openGauss-connector-go-pq"
	_ "github.com/lib/pq"
)

// targetDriverName maps supported targets to the
// database/sql driver they require. An empty type keeps legacy callers on
// PostgreSQL.
func targetDriverName(dbType string) string {
	if dbType == "dameng" {
		return "dm"
	}
	if dbType == "gaussdb" {
		return "opengauss"
	}
	if dbType == "highgo" {
		return "highgo"
	}
	return "postgres"
}

// targetApplier owns a target's SQL and transaction semantics. The runner only
// deals with source transactions and durable positions, never target catalogs.
type targetApplier interface {
	Close() error
	Apply(context.Context, []Change, Position) (Stats, error)
	LoadCheckpoint(context.Context) (Position, bool, error)
	SaveCheckpoint(context.Context, Position) error
	LoadBootstrapRecord(context.Context) (BootstrapRecord, bool, error)
	SaveBootstrapRecord(context.Context, BootstrapRecord) error
	FinalizeBootstrap(context.Context, BootstrapRecord) error
	MarkBootstrapAborted(context.Context) error
	SyncSequences(context.Context, []TableInfo) error
	CheckBusinessTriggers(context.Context, []TableInfo) error
}

func newTargetApplier(ctx context.Context, cfg Config) (targetApplier, error) {
	if cfg.TargetDBType == "dameng" {
		return newDamengApplier(ctx, cfg)
	}
	return newPostgresApplier(ctx, cfg)
}

func targetSchemaExists(ctx context.Context, db *sql.DB, cfg Config) (bool, error) {
	if cfg.TargetDBType == "dameng" {
		var count int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?`, targetSchemaName(cfg)).Scan(&count)
		return count > 0, err
	}
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, cfg.TargetSchema).Scan(&exists)
	return exists, err
}

func openTargetDB(cfg Config) (*sql.DB, error) {
	return sql.Open(targetDriverName(cfg.TargetDBType), cfg.TargetDSN)
}
