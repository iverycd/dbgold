package valueconv

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	goora "github.com/sijms/go-ora/v2"
)

// OracleDamengFloat applies the selected target FLOAT representation. Unlike
// NUMBER, FLOAT permits the target's normal rounding, but never overflow/NaN.
func OracleDamengFloat(v any, dt string, decimal bool, bits int) (any, error) {
	if v == nil {
		return nil, nil
	}
	var raw string
	var binary *big.Float
	switch x := v.(type) {
	case string:
		raw = strings.TrimSpace(x)
	case []byte:
		raw = strings.TrimSpace(string(x))
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("%s 不支持非有限浮点值", dt)
		}
		raw = strconv.FormatFloat(float64(x), 'g', -1, 64)
		binary = new(big.Float).SetPrec(512).SetFloat64(float64(x))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("%s 不支持非有限浮点值", dt)
		}
		raw = strconv.FormatFloat(x, 'g', -1, 64)
		binary = new(big.Float).SetPrec(512).SetFloat64(x)
	default:
		return nil, fmt.Errorf("%s 浮点驱动值类型不支持: %T", dt, v)
	}
	if decimal {
		f, _, err := big.ParseFloat(raw, 10, 512, big.ToNearestEven)
		if err != nil || f.IsInf() {
			return nil, fmt.Errorf("无效或非有限浮点值 %q", raw)
		}
		if binary != nil {
			f = binary
		}
		// DM's decimal wire representation supports at most 38 significant
		// digits. Round only this floating path, never exact NUMBER values.
		rounded := f.Text('g', 38)
		return oracleDamengDecimal(rounded)
	}
	if bits != 32 && bits != 64 {
		return nil, fmt.Errorf("未知达梦浮点位数 %d", bits)
	}
	f, err := strconv.ParseFloat(raw, bits)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%s 浮点值 %q 非法或超出目标范围", dt, raw)
	}
	// ParseFloat underflows to zero without reporting an error.
	if f == 0 {
		if exact, ok := new(big.Rat).SetString(raw); !ok || exact.Sign() != 0 {
			return nil, fmt.Errorf("%s 浮点值 %q 下溢，禁止归零", dt, raw)
		}
	}
	if bits == 32 {
		return float64(float32(f)), nil
	}
	return f, nil
}

var oracleDMPhysicalRowIDAlphabet = regexp.MustCompile(`^[A-Za-z0-9+/.]+$`)
var oracleDMUniversalRowIDAlphabet = regexp.MustCompile(`^[A-Za-z0-9+/.*()=\-]+$`)

func oracleDMRowID(raw string, physical bool) ([]byte, error) {
	if physical && len(raw) != 18 || !physical && (len(raw) == 0 || len(raw) > 4000) {
		return nil, fmt.Errorf("ROWID/UROWID 字符表示长度不合法: %d", len(raw))
	}
	// Preserve restricted ROWID dot separators and logical/foreign UROWID
	// discriminators. These bytes never become a target physical row pointer.
	alphabet := oracleDMUniversalRowIDAlphabet
	if physical {
		alphabet = oracleDMPhysicalRowIDAlphabet
	}
	if !alphabet.MatchString(raw) {
		return nil, fmt.Errorf("ROWID/UROWID 不是有效的 ASCII 标识")
	}
	return []byte(raw), nil
}

func oracleDMBFile(v any) (any, error) {
	var dir, file string
	switch x := v.(type) {
	case goora.BFile:
		if !x.Valid {
			return nil, nil
		}
		dir, file = x.GetDirName(), x.GetFileName()
	case *goora.BFile:
		if x == nil || !x.Valid {
			return nil, nil
		}
		dir, file = x.GetDirName(), x.GetFileName()
	case interface {
		GetDirName() string
		GetFileName() string
	}:
		dir, file = x.GetDirName(), x.GetFileName()
	default:
		return nil, fmt.Errorf("BFILE 需要源端文件引用，收到 %T", v)
	}
	if dir == "" || file == "" || len(dir) > 128 || len(file) > 256 || strings.ContainsAny(dir+file, ":\x00") {
		return nil, fmt.Errorf("BFILE 目录/文件名为空、超长或包含不支持的分隔符")
	}
	// dm's BFILE binding uses a directory:file descriptor; it does not read
	// or copy the external file. The matching target DIRECTORY must exist.
	return dir + ":" + file, nil
}

func validateOracleDMXML(raw string) error {
	decoder := xml.NewDecoder(strings.NewReader(raw))
	hasElement := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !hasElement {
				return fmt.Errorf("XMLTYPE 缺少有效 XML 内容")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("XMLTYPE 序列化内容无效: %w", err)
		}
		if _, ok := token.(xml.StartElement); ok {
			hasElement = true
		}
	}
}

func oracleDMSpatialType(dt string) bool {
	return dt == "MDSYS.SDO_GEOMETRY" || dt == "SDO_GEOMETRY" || dt == "SDE.ST_GEOMETRY" || strings.HasPrefix(dt, "SYSGEO.ST_")
}

var oracleDMEWKTPrefix = regexp.MustCompile(`(?i)^SRID=([0-9]+);(POINT|LINESTRING|POLYGON|MULTIPOINT|MULTILINESTRING|MULTIPOLYGON|GEOMETRYCOLLECTION)\b`)
var oracleDMWKTNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)

// The initial spatial codec deliberately accepts linear two-dimensional EWKT.
// Unsupported dimensions/curves are rejected before the current page commits.
func validateOracleDMEWKT(raw string) error {
	if len(raw) > 32767 {
		return fmt.Errorf("空间 EWKT 超出当前原生文本绑定范围（32767 字节）")
	}
	match := oracleDMEWKTPrefix.FindStringSubmatch(raw)
	if len(match) == 0 {
		return fmt.Errorf("空间对象需要有效 SRID 和受支持的二维线性几何")
	}
	if _, err := strconv.ParseInt(match[1], 10, 32); err != nil {
		return fmt.Errorf("空间对象 SRID 超出目标范围")
	}
	s := strings.TrimSpace(raw[len(match[0]):])
	if strings.EqualFold(s, "EMPTY") {
		return nil
	}
	if s == "" || s[0] != '(' {
		return fmt.Errorf("空间对象暂不支持 Z/M 维度或曲线")
	}
	depth, ordinates, count := 0, 0, 0
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\n\r")
		if s == "" {
			break
		}
		switch s[0] {
		case '(':
			if ordinates != 0 {
				return fmt.Errorf("空间坐标格式无效")
			}
			depth++
		case ')', ',':
			if ordinates != 0 && ordinates != 2 {
				return fmt.Errorf("空间对象仅支持二维坐标")
			}
			ordinates = 0
			if s[0] == ')' {
				depth--
				if depth < 0 {
					return fmt.Errorf("空间对象括号不匹配")
				}
			}
		default:
			if depth == 0 {
				return fmt.Errorf("空间对象包含多余内容")
			}
			if n := oracleDMWKTNumber.FindString(s); n != "" {
				f, err := strconv.ParseFloat(n, 64)
				if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
					return fmt.Errorf("空间坐标非法或超出目标范围")
				}
				ordinates++
				count++
				s = s[len(n):]
				continue
			}
			// A collection has nested type names as well as parentheses.
			upper := strings.ToUpper(s)
			found := false
			for _, name := range []string{"MULTILINESTRING", "GEOMETRYCOLLECTION", "MULTIPOLYGON", "MULTIPOINT", "LINESTRING", "POLYGON", "POINT", "EMPTY"} {
				if strings.HasPrefix(upper, name) {
					s = s[len(name):]
					found = true
					break
				}
			}
			if found {
				continue
			}
			return fmt.Errorf("空间对象包含未支持的维度、曲线或内容")
		}
		s = s[1:]
	}
	if depth != 0 || ordinates != 0 || count == 0 {
		return fmt.Errorf("空间对象坐标或括号不完整")
	}
	return nil
}
