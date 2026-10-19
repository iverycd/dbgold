package valueconv

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/typemap"
	"gitee.com/chunanyong/dm"
)

var oracleDMYearMonth = regexp.MustCompile(`^[+-]?[0-9]{1,9}-[0-9]{2}$`)
var oracleDMDaySecond = regexp.MustCompile(`^[+-]?[0-9]{1,9} [0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?$`)

// OracleDameng keeps exact numbers out of dm's string-to-number float path and
// timestamps out of its time.Time-to-DATETIME(6) and local-timezone path.
func OracleDameng(v interface{}, dt string) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	base := typemap.OracleDamengBaseType(dt)
	text := func() (string, error) {
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
		return "", fmt.Errorf("%s 需要文本驱动值，收到 %T", dt, v)
	}
	switch base {
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC", "FLOAT", "REAL", "DOUBLE PRECISION", "INTEGER", "INT", "SMALLINT":
		raw, err := text()
		if err != nil {
			return nil, err
		}
		return oracleDamengDecimal(raw)
	case "BINARY_FLOAT", "BINARY_DOUBLE":
		switch x := v.(type) {
		case float32:
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, fmt.Errorf("达梦暂不支持非有限浮点值")
			}
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, fmt.Errorf("达梦暂不支持非有限浮点值")
			}
		default:
			return nil, fmt.Errorf("%s 驱动值类型不支持: %T", dt, v)
		}
		return v, nil
	case "DATE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		withTZ := strings.Contains(base, "TIME ZONE")
		var raw string
		if t, ok := v.(time.Time); ok {
			if withTZ {
				_, offset := t.Zone()
				if offset%60 != 0 {
					return nil, fmt.Errorf("达梦时区偏移须为整分钟")
				}
			}
			raw = t.Format("2006-01-02 15:04:05.000000000")
			if withTZ {
				raw += " " + t.Format("-07:00")
			}
		} else {
			var err error
			raw, err = text()
			if err != nil {
				return nil, err
			}
		}
		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(raw, "+")
		if dot := strings.IndexByte(raw, '.'); dot >= 0 {
			parts := strings.Fields(raw[dot+1:])
			if len(parts) == 0 {
				return nil, fmt.Errorf("无效 %s 日期 %q", dt, raw)
			}
			fraction := parts[0]
			if len(fraction) > 9 {
				return nil, fmt.Errorf("%s 值超过 9 位小数秒", dt)
			}
		}
		layout := "2006-01-02 15:04:05.999999999"
		if withTZ {
			layout += " -07:00"
		}
		t, err := time.Parse(layout, strings.TrimSpace(raw))
		if err != nil || t.Year() < 1 || t.Year() > 9999 {
			return nil, fmt.Errorf("无效或不支持的 %s 日期 %q", dt, raw)
		}
		if t.Year() == 1582 && t.Month() == 10 && t.Day() >= 5 && t.Day() <= 14 {
			return nil, fmt.Errorf("达梦会归一化 1582-10-05 至 1582-10-14，禁止静默修改日期")
		}
		if base == "DATE" {
			if t.Nanosecond() != 0 {
				return nil, fmt.Errorf("Oracle DATE 含非零小数秒")
			}
			return t.Format("2006-01-02 15:04:05"), nil
		}
		if withTZ {
			_, offset := t.Zone()
			if offset < -(12*60+59)*60 || offset > 14*3600 {
				return nil, fmt.Errorf("时区偏移超出达梦范围")
			}
			return t.Format("2006-01-02 15:04:05.000000000 -07:00"), nil
		}
		return t.Format("2006-01-02 15:04:05.000000000"), nil
	case "INTERVAL YEAR TO MONTH":
		raw, err := text()
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(raw)
		if !oracleDMYearMonth.MatchString(raw) {
			return nil, fmt.Errorf("无效年月区间 %q", raw)
		}
		return dm.NewDmIntervalYMByString("INTERVAL '" + raw + "' YEAR(9) TO MONTH")
	case "INTERVAL DAY TO SECOND":
		raw, err := text()
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(raw)
		if !oracleDMDaySecond.MatchString(raw) {
			return nil, fmt.Errorf("无效日秒区间 %q", raw)
		}
		if dot := strings.LastIndexByte(raw, '.'); dot >= 0 {
			fraction := raw[dot+1:]
			if len(fraction) > 6 {
				if strings.Trim(fraction[6:], "0") != "" {
					return nil, fmt.Errorf("达梦区间最多支持 6 位小数秒，禁止截断")
				}
				raw = raw[:dot+7]
			}
		}
		interval, err := dm.NewDmIntervalDTByString("INTERVAL '" + raw + "' DAY(9) TO SECOND(6)")
		if err != nil {
			return nil, err
		}
		fraction := 0
		if dot := strings.LastIndexByte(raw, '.'); dot >= 0 {
			part := raw[dot+1:]
			part += strings.Repeat("0", 6-len(part))
			fraction, _ = strconv.Atoi(part)
		}
		// The driver's parser uses int(float64(fraction)*1e6), which can
		// turn e.g. .000249 into 248 microseconds. Refuse that lossy binding.
		if interval.GetMsec() != fraction {
			return nil, fmt.Errorf("达梦驱动区间绑定无法精确保留小数秒 %q", raw)
		}
		return interval, nil
	case "CHAR", "VARCHAR", "VARCHAR2", "NCHAR", "NVARCHAR2", "CLOB", "NCLOB", "LONG":
		return text()
	case "RAW", "LONG RAW", "BLOB":
		if b, ok := v.([]byte); ok {
			return b, nil
		}
		return nil, fmt.Errorf("%s 需要二进制驱动值，收到 %T", dt, v)
	}
	return nil, fmt.Errorf("不支持 Oracle 类型 %s", dt)
}

func oracleDamengDecimal(raw string) (interface{}, error) {
	raw = strings.TrimSpace(raw)
	want, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, fmt.Errorf("无效十进制数 %q", raw)
	}
	abs := new(big.Rat).Abs(want)
	min, _ := new(big.Rat).SetString("1e-128")
	max, _ := new(big.Rat).SetString("1e126")
	if want.Sign() < 0 {
		max, _ = new(big.Rat).SetString("1e124")
	}
	if abs.Sign() != 0 && (abs.Cmp(min) < 0 || abs.Cmp(max) >= 0) {
		return nil, fmt.Errorf("数值 %q 超出达梦 NUMBER 范围，禁止截断或归零", raw)
	}
	f, ok := new(big.Float).SetPrec(512).SetString(raw)
	if !ok {
		return nil, fmt.Errorf("无效十进制数 %q", raw)
	}
	dec, err := dm.NewDecimalFromBigFloat(f)
	if err != nil {
		return nil, err
	}
	got, ok := new(big.Rat).SetString(dec.String())
	if !ok || got.Cmp(want) != 0 {
		return nil, fmt.Errorf("达梦十进制绑定无法精确保留 %q", raw)
	}
	// dm encodes negative values in at most 19 base-100 mantissa bytes.
	// Odd decimal scale needs a padding digit at both ends, leaving only 37
	// significant digits. String() roundtrips alone do not detect this wire loss.
	plain := strings.TrimPrefix(dec.String(), "-")
	scale := 0
	if dot := strings.IndexByte(plain, '.'); dot >= 0 {
		scale = len(plain) - dot - 1
		plain = strings.ReplaceAll(plain, ".", "")
	}
	plain = strings.TrimLeft(plain, "0")
	trimmed := strings.TrimRight(plain, "0")
	scale -= len(plain) - len(trimmed)
	if len(trimmed) > 38 || want.Sign() < 0 && len(trimmed) == 38 && scale%2 != 0 {
		return nil, fmt.Errorf("达梦驱动无法无损编码数值 %q 的有效精度", raw)
	}
	return dec, nil
}

func OracleDamengPlaceholder(dt string) string {
	switch typemap.OracleDamengBaseType(dt) {
	case "DATE":
		return "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS')"
	case "TIMESTAMP":
		return "TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9')"
	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		return "TO_TIMESTAMP_TZ(?, 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')"
	default:
		return "?"
	}
}
