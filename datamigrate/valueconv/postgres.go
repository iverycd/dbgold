package valueconv

import (
	"encoding/hex"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// 时间格式常量,与各 Reader 历史行为逐字符一致。
const (
	dateTimeLayout = "2006-01-02 15:04:05.999999"
	timeLayout     = "15:04:05.999999"
)

// PostgresValueConverter 把中立值落地为 PostgreSQL(pq COPY 协议)能接受的形态。
// 它精确复刻重构前各 Reader.ReadPage 中「PG 定制」的那部分转换,
// 按 srcType 区分(同一 time.Time 在 MySQL 现状不转、在 SqlServer 转字符串)。
//
// 注意:通用清洗(去 \x00、SqlServer bit bool→int64、二进制保持 []byte)
// 仍由 Reader 完成,本 Converter 不重复处理。
type PostgresValueConverter struct{}

func NewPostgres() *PostgresValueConverter { return &PostgresValueConverter{} }

func (c *PostgresValueConverter) Convert(val interface{}, srcType, dbTypeName string) interface{} {
	if val == nil {
		return nil
	}
	switch srcType {
	case "mysql":
		return convertMySQL(val, dbTypeName)
	case "sqlserver":
		return convertSQLServer(val, dbTypeName)
	case "oracle":
		return convertOracle(val, dbTypeName)
	case "dameng":
		return convertDaMeng(val, dbTypeName)
	default:
		return val
	}
}

// convertMySQL 复刻 mysql.go ReadPage(L242-258)的 PG 定制部分。
// BIT/GEOMETRY 的中立值是 []byte,在此转为 PG 文本形态。
func convertMySQL(val interface{}, dt string) interface{} {
	b, ok := val.([]byte)
	if !ok {
		return val
	}
	switch dt {
	case "BIT":
		return hex.EncodeToString(b)[1:]
	case "GEOMETRY":
		return hex.EncodeToString(b)[8:]
	default:
		return val
	}
}

// convertSQLServer 复刻 sqlserver.go ReadPage(L239-269)的 PG 定制部分。
func convertSQLServer(val interface{}, dt string) interface{} {
	switch dt {
	case "UNIQUEIDENTIFIER":
		if uid, ok := val.(mssql.UniqueIdentifier); ok {
			return uid.String()
		}
	case "DATETIME", "DATETIME2", "SMALLDATETIME", "DATE":
		if t, ok := val.(time.Time); ok {
			return t.Format(dateTimeLayout)
		}
	case "TIME":
		if t, ok := val.(time.Time); ok {
			return t.Format(timeLayout)
		}
	case "MONEY", "SMALLMONEY":
		if b, ok := val.([]byte); ok {
			return string(b)
		}
	case "XML":
		if x, ok := val.([]byte); ok {
			return string(x)
		}
	}
	return val
}

// convertOracle 复刻 oracle.go ReadPage 的 PG 定制部分(日期/时间格式化)。
func convertOracle(val interface{}, dt string) interface{} {
	if dt == "DATE" || strings.HasPrefix(dt, "TIMESTAMP") {
		if t, ok := val.(time.Time); ok {
			return t.Format(dateTimeLayout)
		}
	}
	return val
}

// convertDaMeng 复刻 dameng.go ReadPage(L235-241)的 PG 定制部分。
func convertDaMeng(val interface{}, dt string) interface{} {
	switch dt {
	case "DATE", "DATETIME", "TIMESTAMP":
		if t, ok := val.(time.Time); ok {
			return t.Format(dateTimeLayout)
		}
	case "TIME":
		if t, ok := val.(time.Time); ok {
			return t.Format(timeLayout)
		}
	}
	return val
}
