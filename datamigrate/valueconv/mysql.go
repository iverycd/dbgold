package valueconv

import (
	"encoding/hex"

	mssql "github.com/microsoft/go-mssqldb"
)

// MySQLValueConverter 把 Reader 输出的中立值落地为 MySQL(go-sql-driver)能接受的形态。
//
// go-sql-driver 通过参数化 INSERT 接收值,能直接接受 time.Time(连接串 parseTime=true)、
// []byte(BLOB/BINARY)、数值与字符串,大部分类型无需转换。
// 仅少数源库的特殊中立值需要落地:
//   - MySQL 源 BIT/GEOMETRY(中立值为 []byte)
//   - SQL Server 源 UNIQUEIDENTIFIER/XML/MONEY
type MySQLValueConverter struct{}

func NewMySQL() *MySQLValueConverter { return &MySQLValueConverter{} }

func (c *MySQLValueConverter) Convert(val interface{}, srcType, dbTypeName string) interface{} {
	if val == nil {
		return nil
	}
	switch srcType {
	case "mysql":
		return mysqlTargetFromMySQL(val, dbTypeName)
	case "sqlserver":
		return mysqlTargetFromSQLServer(val, dbTypeName)
	case "oracle", "dameng":
		// time.Time、[]byte、数值、字符串均可直接交给驱动
		return val
	default:
		return val
	}
}

// mysqlTargetFromMySQL 处理 MySQL → MySQL 的特殊中立值。
func mysqlTargetFromMySQL(val interface{}, dt string) interface{} {
	b, ok := val.([]byte)
	if !ok {
		return val
	}
	switch dt {
	case "BIT":
		// 目标为 tinyint(1):位串字节转十进制整数
		return bytesToIntMySQL(b)
	case "GEOMETRY":
		// 目标为 longtext:用十六进制串表示(去掉前 4 字节 SRID)
		if len(b) >= 4 {
			return hex.EncodeToString(b)[8:]
		}
		return hex.EncodeToString(b)
	default:
		return val
	}
}

// mysqlTargetFromSQLServer 处理 SQL Server → MySQL 的特殊中立值。
func mysqlTargetFromSQLServer(val interface{}, dt string) interface{} {
	switch dt {
	case "UNIQUEIDENTIFIER":
		if uid, ok := val.(mssql.UniqueIdentifier); ok {
			return uid.String()
		}
	case "MONEY", "SMALLMONEY":
		if b, ok := val.([]byte); ok {
			return string(b)
		}
	case "XML":
		if x, ok := val.([]byte); ok {
			return string(x)
		}
	case "DATETIME", "DATETIME2", "SMALLDATETIME", "DATE", "TIME", "DATETIMEOFFSET":
		// go-sql-driver 接受 time.Time,无需格式化
		return val
	}
	return val
}

// bytesToIntMySQL 把大端字节序列转为整数(用于 MySQL BIT → tinyint)。
func bytesToIntMySQL(b []byte) int64 {
	var n int64
	for _, x := range b {
		n = n<<8 | int64(x)
	}
	return n
}
