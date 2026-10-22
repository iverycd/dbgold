package validation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxValueBytes bounds work on a single materialized field in addition to the
// sort buffer. A driver can additionally retain its current row internally.
const MaxValueBytes = 16 << 20

type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string { return e.Reason }
func unsupported(format string, args ...any) error {
	return &UnsupportedError{fmt.Sprintf(format, args...)}
}

var decimalPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func canonical(value any, logical, native Column) ([]byte, error) {
	if logical.Kind == Unsupported || logical.Kind == "" || native.Kind == Unsupported || native.Kind == "" {
		return nil, unsupported("列 %s 的类型 %s 缺少无损校验器", logical.Name, native.DatabaseType)
	}
	if value == nil {
		return []byte{0}, nil
	}
	if b, ok := value.([]byte); ok && len(b) > MaxValueBytes {
		return nil, unsupported("列 %s 超过单字段校验上限 %d MiB", logical.Name, MaxValueBytes>>20)
	}
	if s, ok := value.(string); ok && len(s) > MaxValueBytes {
		return nil, unsupported("列 %s 超过单字段校验上限 %d MiB", logical.Name, MaxValueBytes>>20)
	}
	var payload string
	var family string
	var err error
	switch logical.Kind {
	case Text:
		family = "text"
		payload, err = scalarText(value)
		if err == nil && !utf8.ValidString(payload) {
			err = unsupported("文本不是有效 UTF-8，无法无损校验")
		}
	case Binary:
		family = "bytes"
		b, ok := value.([]byte)
		if !ok {
			return nil, unsupported("列 %s 的二进制值未以原始字节返回", logical.Name)
		}
		payload = string(b)
	case Number, Float:
		family = "number"
		payload, err = canonicalNumber(value, native)
	case Bool:
		family = "number"
		if b, ok := value.(bool); ok {
			if b {
				payload = "1"
			} else {
				payload = "0"
			}
		} else {
			s, e := scalarText(value)
			if e == nil && (strings.EqualFold(s, "true") || s == "t") {
				payload = "1"
			} else if e == nil && (strings.EqualFold(s, "false") || s == "f") {
				payload = "0"
			} else {
				payload, err = canonicalNumber(value, native)
			}
		}
	case Bits:
		family = "number"
		if native.Kind == Number || native.Kind == Float || native.Kind == Bool {
			payload, err = canonicalNumber(value, native)
		} else {
			s, e := scalarText(value)
			if e != nil {
				err = e
				break
			}
			if len(s) == 0 || strings.Trim(s, "01") != "" {
				err = unsupported("BIT 未以完整 0/1 位串返回")
				break
			}
			n, ok := new(big.Int).SetString(s, 2)
			if !ok {
				err = unsupported("BIT 无法解析")
				break
			}
			payload = n.String()
		}
	case UUID:
		family = "uuid"
		var id uuid.UUID
		if b, ok := value.([]byte); ok && len(b) == 16 && native.Kind == Binary {
			id, err = uuid.FromBytes(b)
		} else {
			var s string
			s, err = scalarText(value)
			if err == nil {
				id, err = uuid.Parse(s)
			}
		}
		if err != nil {
			err = unsupported("UUID 无法无损解析")
		} else {
			payload = id.String()
		}
	case Date, Timestamp:
		family = "wall_time"
		if native.Kind == TimestampTZ {
			err = unsupported("无时区源值不能与带时区目标值安全等同")
			break
		}
		payload, err = canonicalTimestamp(value, false)
	case TimestampTZ:
		family = "instant"
		if native.Kind == Timestamp || native.Kind == Date {
			err = unsupported("目标无时区类型无法证明源时间瞬间被完整保留")
			break
		}
		payload, err = canonicalTimestamp(value, true)
	case Time:
		family = "time"
		payload, err = canonicalTime(value)
	case JSON:
		family = "json"
		var s string
		s, err = scalarText(value)
		if err == nil {
			payload, err = canonicalJSON(s)
		}
	default:
		err = unsupported("类型 %s 没有内容规范化规则", logical.Kind)
	}
	if err != nil {
		return nil, unsupported("列 %s 无法完成内容校验: %s", logical.Name, err.Error())
	}
	// The family is separated from payload; row serialization also length-prefixes
	// each canonical cell, so NULL, empty strings and embedded NUL cannot collide.
	return []byte(family + "\x00" + payload), nil
}

func scalarText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case bool:
		return strconv.FormatBool(x), nil
	case json.Number:
		return string(x), nil
	default:
		return "", unsupported("驱动返回了不能安全解释的值类型 %T", v)
	}
}

func canonicalNumber(v any, native Column) (string, error) {
	if b, ok := v.(bool); ok {
		if b {
			return "1", nil
		}
		return "0", nil
	}
	if native.Kind == Float {
		bits := native.FloatBits
		if bits != 32 && bits != 64 {
			return "", unsupported("原生浮点精度未知")
		}
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case float32:
			n = float64(x)
		default:
			s, e := scalarText(v)
			if e != nil {
				return "", e
			}
			var e2 error
			n, e2 = strconv.ParseFloat(s, bits)
			if e2 != nil {
				return "", unsupported("浮点值无法解析")
			}
		}
		if bits == 32 {
			n = float64(float32(n))
		}
		if math.IsNaN(n) {
			return "NaN", nil
		}
		if math.IsInf(n, 1) {
			return "+Inf", nil
		}
		if math.IsInf(n, -1) {
			return "-Inf", nil
		}
		return ratText(new(big.Rat).SetFloat64(n)), nil
	}
	// Exact database numbers must never already have passed through float64.
	switch v.(type) {
	case float32, float64:
		return "", unsupported("精确数值被驱动转成浮点，无法证明没有精度损失")
	}
	s, e := scalarText(v)
	if e != nil {
		return "", e
	}
	return canonicalDecimal(s)
}

func canonicalDecimal(s string) (string, error) {
	if !decimalPattern.MatchString(s) {
		return "", unsupported("精确数值不是完整十进制文本")
	}
	if p := strings.IndexAny(s, "eE"); p >= 0 {
		exp, e := strconv.ParseInt(s[p+1:], 10, 32)
		if e != nil || exp > 200000 || exp < -200000 {
			return "", unsupported("数值指数超过校验资源上限")
		}
	}
	if len(s) > 200000 {
		return "", unsupported("数值位数超过校验资源上限")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", unsupported("精确数值无法解析")
	}
	return ratText(r), nil
}
func ratText(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	return r.Num().String() + "/" + r.Denom().String()
}

func canonicalTimestamp(v any, zoned bool) (string, error) {
	if t, ok := v.(time.Time); ok {
		if zoned {
			t = t.UTC()
		}
		return t.Format("2006-01-02T15:04:05.000000000"), nil
	}
	s, e := scalarText(v)
	if e != nil {
		return "", e
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "+")
	// time.Parse otherwise silently drops fractional digits after nanoseconds.
	// Keep equivalent trailing zeroes, but never discard a nonzero digit.
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		end := dot + 1
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		fraction := strings.TrimRight(s[dot+1:end], "0")
		if len(fraction) > 9 {
			return "", unsupported("日期时间超过纳秒精度，禁止静默截断")
		}
		if end-dot-1 > 9 {
			s = s[:dot+1] + s[dot+1:dot+10] + s[end:]
		}
	}
	if !zoned && isZeroDate(s) {
		s = strings.ReplaceAll(s, " ", "T")
		if p := strings.IndexByte(s, '.'); p >= 0 {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		if len(s) == 10 {
			s += "T00:00:00"
		}
		return "zero:" + s, nil
	}
	layouts := []string{"2006-01-02", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"}
	if zoned {
		layouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -07:00", "2006-01-02T15:04:05.999999999 -07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999-07", "2006-01-02T15:04:05.999999999-07", "2006-01-02 15:04:05.999999999Z07:00:00"}
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			if zoned {
				t = t.UTC()
			}
			return t.Format("2006-01-02T15:04:05.000000000"), nil
		}
	}
	return "", unsupported("日期时间无法保留全部精度或时区")
}
func isZeroDate(s string) bool {
	return len(s) >= 10 && s[4] == '-' && s[7] == '-' && (s[:4] == "0000" || s[5:7] == "00" || s[8:10] == "00")
}

func canonicalTime(v any) (string, error) {
	if t, ok := v.(time.Time); ok {
		return strconv.FormatInt(int64(t.Hour())*3600*1e9+int64(t.Minute())*60*1e9+int64(t.Second())*1e9+int64(t.Nanosecond()), 10), nil
	}
	s, e := scalarText(v)
	if e != nil {
		return "", e
	}
	sign := int64(1)
	if strings.HasPrefix(s, "-") {
		sign = -1
		s = s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	p := strings.Split(s, ":")
	if len(p) != 3 {
		return "", unsupported("TIME 格式无法解析")
	}
	h, e1 := strconv.ParseInt(p[0], 10, 32)
	m, e2 := strconv.ParseInt(p[1], 10, 32)
	sec, e3 := canonicalDecimal(p[2])
	if e1 != nil || e2 != nil || e3 != nil || h < 0 || m < 0 || m > 59 {
		return "", unsupported("TIME 格式无法解析")
	}
	r, ok := new(big.Rat).SetString(sec)
	if !ok || r.Sign() < 0 || r.Cmp(big.NewRat(60, 1)) >= 0 {
		return "", unsupported("TIME 秒值无法解析")
	}
	r.Mul(r, big.NewRat(1e9, 1))
	if !r.IsInt() {
		return "", unsupported("TIME 超过纳秒精度")
	}
	hours := new(big.Int).Mul(big.NewInt(h), big.NewInt(3600*1e9))
	hours.Add(hours, big.NewInt(m*60*1e9))
	r.Add(r, new(big.Rat).SetInt(hours))
	r.Mul(r, big.NewRat(sign, 1))
	return ratText(r), nil
}

func canonicalJSON(s string) (string, error) {
	if !utf8.ValidString(s) || !json.Valid([]byte(s)) || !validJSONSurrogates(s) {
		return "", unsupported("JSON 不是可无损解释的 Unicode JSON")
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	b, e := jsonValue(d, 0)
	if e != nil {
		return "", e
	}
	if _, e = d.Token(); e != io.EOF {
		return "", unsupported("JSON 末尾有额外内容")
	}
	return string(b), nil
}
func jsonValue(d *json.Decoder, depth int) ([]byte, error) {
	if depth > 512 {
		return nil, unsupported("JSON 嵌套超过校验上限")
	}
	t, e := d.Token()
	if e != nil {
		return nil, unsupported("JSON 解析失败")
	}
	switch x := t.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if x {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		b, _ := json.Marshal(x)
		return b, nil
	case json.Number:
		n, e := canonicalDecimal(string(x))
		if e != nil {
			return nil, e
		}
		return []byte("number:" + n), nil
	case json.Delim:
		var out bytes.Buffer
		if x == '[' {
			out.WriteByte('[')
			first := true
			for d.More() {
				v, e := jsonValue(d, depth+1)
				if e != nil {
					return nil, e
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				out.Write(v)
			}
			if _, e = d.Token(); e != nil {
				return nil, unsupported("JSON 数组未结束")
			}
			out.WriteByte(']')
			return out.Bytes(), nil
		}
		if x == '{' {
			type pair struct {
				k string
				v []byte
			}
			var items []pair
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, unsupported("JSON 对象键无法解析")
				}
				name, ok := k.(string)
				if !ok {
					return nil, unsupported("JSON 对象键不是文本")
				}
				if seen[name] {
					return nil, unsupported("JSON 含重复对象键，无法安全判断等价")
				}
				seen[name] = true
				v, e := jsonValue(d, depth+1)
				if e != nil {
					return nil, e
				}
				items = append(items, pair{name, v})
			}
			if _, e = d.Token(); e != nil {
				return nil, unsupported("JSON 对象未结束")
			}
			sort.Slice(items, func(i, j int) bool { return items[i].k < items[j].k })
			out.WriteByte('{')
			for i, p := range items {
				if i > 0 {
					out.WriteByte(',')
				}
				k, _ := json.Marshal(p.k)
				out.Write(k)
				out.WriteByte(':')
				out.Write(p.v)
			}
			out.WriteByte('}')
			return out.Bytes(), nil
		}
	}
	return nil, unsupported("JSON token 无法解释")
}
func validJSONSurrogates(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		i++
		if i >= len(s) {
			return false
		}
		if s[i] != 'u' {
			continue
		}
		if i+4 >= len(s) {
			return false
		}
		n, e := strconv.ParseUint(s[i+1:i+5], 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(s) || s[i+1] != '\\' || s[i+2] != 'u' {
				return false
			}
			lo, e := strconv.ParseUint(s[i+3:i+7], 16, 16)
			if e != nil || lo < 0xdc00 || lo > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
