package valueconv

import (
	"fmt"
	"gitee.com/chunanyong/dm"
	mssql "github.com/microsoft/go-mssqldb"
	"math/big"
	"strings"
	"time"
)

// SQLServerDameng validates the driver's neutral values. Decimal text must not
// pass through float64; GUID bytes use SQL Server's mixed-endian representation.
func SQLServerDameng(v interface{}, dt string) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	dt = strings.ToUpper(dt)
	switch dt {
	case "UNIQUEIDENTIFIER":
		var id mssql.UniqueIdentifier
		switch x := v.(type) {
		case mssql.UniqueIdentifier:
			return x.String(), nil
		case []byte, string:
			if err := id.Scan(x); err != nil {
				return nil, err
			}
			return id.String(), nil
		}
		return nil, fmt.Errorf("UUID 驱动值类型不支持: %T", v)
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		var raw string
		switch v := v.(type) {
		case []byte:
			raw = string(v)
		case string:
			raw = v
		default:
			return nil, fmt.Errorf("%s 需要精确十进制驱动值，收到 %T", dt, v)
		}
		// dm's string->NUMBER path uses a 64-bit big.Float, losing digits beyond
		// roughly 19 decimal digits. Bind DmDecimal directly with a checked roundtrip.
		f, ok := new(big.Float).SetPrec(256).SetString(raw)
		if !ok {
			return nil, fmt.Errorf("无效十进制数 %q", raw)
		}
		dec, err := dm.NewDecimalFromBigFloat(f)
		if err != nil {
			return nil, err
		}
		want, ok1 := new(big.Rat).SetString(raw)
		got, ok2 := new(big.Rat).SetString(dec.String())
		if !ok1 || !ok2 || want.Cmp(got) != 0 {
			return nil, fmt.Errorf("达梦十进制绑定无法精确保留 %q", raw)
		}
		return dec, nil
	case "XML", "CHAR", "NCHAR", "VARCHAR", "NVARCHAR", "TEXT", "NTEXT":
		if b, ok := v.([]byte); ok {
			return string(b), nil
		}
		return v, nil
	case "BIT":
		if b, ok := v.(bool); ok {
			if b {
				return int64(1), nil
			}
			return int64(0), nil
		}
		return v, nil
	case "DATETIMEOFFSET", "DATETIME2", "TIME", "DATE", "DATETIME", "SMALLDATETIME":
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("%s 驱动值类型不支持: %T", dt, v)
		}
		if dt != "DATETIME" && t.Nanosecond()%1000 != 0 {
			return nil, fmt.Errorf("%s 值超过 6 位小数秒", dt)
		}
		switch dt {
		case "DATETIMEOFFSET":
			return t.Format("2006-01-02 15:04:05.000000 -07:00"), nil
		case "TIME":
			return t.Format("15:04:05.000000"), nil
		case "DATE":
			return t.Format("2006-01-02"), nil
		case "DATETIME":
			return t.Round(time.Millisecond).Format("2006-01-02 15:04:05.000000"), nil
		default:
			return t.Format("2006-01-02 15:04:05.000000"), nil
		}
	}
	return v, nil
}

// Explicit datetime parsing avoids dm driver's time.Time -> DATETIME binding
// (which drops offsets), and avoids dependence on the session's NLS formats.
func SQLServerDamengPlaceholder(dt string) string {
	switch strings.ToUpper(dt) {
	case "DATETIMEOFFSET":
		return "TO_TIMESTAMP_TZ(?, 'YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')"
	case "DATETIME", "DATETIME2", "SMALLDATETIME":
		return "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF6')"
	case "DATE":
		return "TO_DATE(?, 'YYYY-MM-DD')"
	case "TIME":
		return "CAST(? AS TIME(6))"
	default:
		return "?"
	}
}
