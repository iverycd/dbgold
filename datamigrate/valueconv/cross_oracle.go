package valueconv

import (
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/source"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/converters"
)

// ConvertCrossOracle retains source metadata; a SQL Server TIMESTAMP is binary,
// while a DM TIMESTAMP is temporal. Values never pass through float64 for NUMBER.
func ConvertCrossOracle(v any, src source.ColumnInfo, product string, dst OracleColumn, codec converters.IStringConverter) (any, string, error) {
	if v == nil {
		if !src.IsNullable {
			return nil, "", fmt.Errorf("非空源列不能接收 NULL")
		}
		return ConvertOracle(v, src.DataType, dst, codec)
	}
	t := strings.ToLower(src.DataType)
	if dst.DataType == "NUMBER" {
		if b, ok := v.(bool); ok {
			if b {
				v = "1"
			} else {
				v = "0"
			}
		}
		p := dst.Precision
		if p == 0 && dst.ScaleKnown {
			p = 38
		}
		n, e := PGOracleNumber(OracleText(v), p, dst.Scale)
		return n, "", e
	}
	if dst.DataType == "RAW" {
		b, ok := v.([]byte)
		if !ok || dst.DataLength > 0 && int64(len(b)) > dst.DataLength {
			return nil, "", fmt.Errorf("RAW 数据不是有效二进制或长度超限")
		}
		if product == "sqlserver" && (t == "rowversion" || t == "timestamp") && len(b) != 8 {
			return nil, "", fmt.Errorf("rowversion 必须为 8 字节")
		}
		if len(b) == 0 {
			return nil, "", fmt.Errorf("Oracle RAW 无法区分空二进制与 NULL，需要 BLOB 表示")
		}
		return append([]byte{}, b...), "", nil
	}
	if product == "sqlserver" && t == "uniqueidentifier" {
		var e error
		v, e = SQLServerDameng(v, "UNIQUEIDENTIFIER")
		if e != nil {
			return nil, "", e
		}
	}
	if t == "time" {
		tm, ok := v.(time.Time)
		if !ok {
			var err error
			tm, err = time.Parse("15:04:05.999999999", OracleText(v))
			if err != nil {
				return nil, "", fmt.Errorf("无效 TIME 值: %w", err)
			}
		}
		v = tm.Format("15:04:05.999999999")
	}
	if strings.Contains(dst.DataType, "WITH TIME ZONE") {
		tm, ok := v.(time.Time)
		if !ok {
			var err error
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -07:00", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-07"} {
				tm, err = time.Parse(layout, OracleText(v))
				if err == nil {
					break
				}
			}
			if err != nil {
				return nil, "", fmt.Errorf("无效带时区时间: %w", err)
			}
		}
		if err := pgOracleTimePrecision(tm, dst.Scale); err != nil {
			return nil, "", err
		}
		return goora.TimeStampTZ(tm), "", nil
	}
	// Character empty values, including NVARCHAR, follow the accepted Oracle
	// policy even when a long source column was mapped to a LOB.
	if OracleText(v) == "" && (strings.Contains(t, "char") || t == "text" || t == "ntext") {
		if !dst.Nullable {
			return nil, "", fmt.Errorf("非空列不能接收空字符串")
		}
		return nil, "空字符串转 NULL", nil
	}
	return ConvertOracle(v, src.DataType, dst, codec)
}
