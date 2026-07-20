package cdc

import (
	"database/sql"

	_ "gitee.com/opengauss/openGauss-connector-go-pq"
	_ "github.com/lib/pq"
)

// targetDriverName maps supported PostgreSQL-compatible targets to the
// database/sql driver they require. An empty type keeps legacy callers on
// PostgreSQL.
func targetDriverName(dbType string) string {
	if dbType == "gaussdb" {
		return "opengauss"
	}
	return "postgres"
}

func openTargetDB(cfg Config) (*sql.DB, error) {
	return sql.Open(targetDriverName(cfg.TargetDBType), cfg.TargetDSN)
}
