package valueconv

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/source"
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

func (c *PostgresValueConverter) Convert(val interface{}, srcType, dbTypeName string) (interface{}, error) {
	if val == nil {
		return nil, nil
	}
	switch srcType {
	case "mysql":
		return convertMySQL(val, dbTypeName)
	case "sqlserver":
		return convertSQLServer(val, dbTypeName), nil
	case "oracle":
		return convertOracle(val, dbTypeName), nil
	case "dameng":
		return convertDaMeng(val, dbTypeName), nil
	default:
		return val, nil
	}
}

// convertMySQL preserves spatial coordinates and SRID in the destination's
// canonical HEXEWKB text input, shared by COPY and CDC parameter binding.
func convertMySQL(val interface{}, dt string) (interface{}, error) {
	if _, spatial := source.MySQLSpatialSubtype(dt); spatial {
		b, ok := val.([]byte)
		if !ok {
			return nil, fmt.Errorf("MySQL 空间值需要原始二进制数据，实际为 %T", val)
		}
		return MySQLSpatialHEXEWKB(b, dt)
	}
	b, ok := val.([]byte)
	if !ok {
		return val, nil
	}
	switch dt {
	case "BIT":
		if len(b) == 0 {
			return nil, fmt.Errorf("MySQL BIT 值为空二进制数据")
		}
		return hex.EncodeToString(b)[1:], nil
	default:
		return val, nil
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
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		if b, ok := val.([]byte); ok {
			// SQL Server returns exact decimal text as []byte. pq COPY treats
			// []byte as bytea, so preserve every digit by binding the text directly.
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
