package source

import mysql "github.com/go-sql-driver/mysql"

// NewMySQLForDameng preserves raw date values and enables DM FULLTEXT handling.
func NewMySQLForDameng(dsn, database string, pool ConnPoolConfig) (*MySQLReader, error) {
	dsn, err := mysqlDamengDSN(dsn)
	if err != nil {
		return nil, err
	}
	r, err := NewMySQL(dsn, database, pool)
	if err != nil {
		return nil, err
	}
	r.damengTarget = true
	return r, nil
}

func mysqlDamengDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	cfg.ParseTime = false
	return cfg.FormatDSN(), nil
}
