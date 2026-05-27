package driver

import (
	"dbgold/driver/gaussdb"
	"dbgold/driver/mysql"
	"dbgold/driver/oracle"
	"dbgold/driver/postgres"
	"dbgold/driver/sqlserver"
	"fmt"
)

func NewDriver(dbType string) (Driver, error) {
	switch dbType {
	case "mysql":
		return mysql.New(), nil
	case "postgres":
		return postgres.New(), nil
	case "oracle":
		return oracle.New(), nil
	case "sqlserver":
		return sqlserver.New(), nil
	case "gaussdb":
		return gaussdb.New(), nil
	default:
		return nil, fmt.Errorf("unsupported db type: %s", dbType)
	}
}
