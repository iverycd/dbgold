package source

import mysql "github.com/go-sql-driver/mysql"

// NewMySQLForDameng preserves raw date values while keeping the existing MySQL
// metadata, value normalization and pagination behavior for all other types.
func NewMySQLForDameng(dsn, database string, pool ConnPoolConfig) (*MySQLReader, error) {
	dsn, err := mysqlDamengDSN(dsn)
	if err != nil {
		return nil, err
	}
	return NewMySQL(dsn, database, pool)
}

func mysqlDamengDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	cfg.ParseTime = false
	return cfg.FormatDSN(), nil
}
