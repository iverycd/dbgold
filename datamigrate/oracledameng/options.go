package oracledameng

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// Options shares the selected Oracle-to-DM SQL semantics between defaults and
// views. Float applies only to a column's numeric DEFAULT literal.
type Options struct {
	UseDate      bool
	CharInLength bool
	UseNvarchar2 bool
	Float        bool
}

func decodeOracleLiteral(raw string) (string, bool, error) {
	ts, err := tokenizeView(raw)
	if err != nil || len(ts) != 1 || ts[0].kind != 's' || ts[0].start != 0 || ts[0].end != len(raw) {
		return "", false, fmt.Errorf("无效 Oracle 字符串字面量 %q", raw)
	}
	s, national := raw, false
	if s[0] == 'n' || s[0] == 'N' {
		national, s = true, s[1:]
	}
	if s[0] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), national, nil
	}
	delim, size := utf8.DecodeRuneInString(s[2:])
	closing := delim
	switch delim {
	case '[':
		closing = ']'
	case '{':
		closing = '}'
	case '(':
		closing = ')'
	case '<':
		closing = '>'
	}
	return s[2+size : len(s)-len(string(closing))-1], national, nil
}

func renderOracleLiteral(raw string) (string, error) {
	value, national, err := decodeOracleLiteral(raw)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "NULL", nil
	}
	if strings.ContainsRune(value, '\\') {
		return "", fmt.Errorf("含反斜杠的 Oracle SQL 字符串暂不支持自动迁移（目标转义设置可能改变原值）")
	}
	qpos := 0
	if national {
		qpos = 1
	}
	if raw[qpos] == '\'' {
		return raw, nil
	}
	prefix := ""
	if national {
		prefix = "N"
	}
	return prefix + "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

var oracleDMCastChar = regexp.MustCompile(`^(CHAR|CHARACTER|VARCHAR|VARCHAR2|CHAR VARYING|CHARACTER VARYING|NCHAR|NVARCHAR2|NATIONAL CHAR|NATIONAL CHARACTER|NCHAR VARYING|NATIONAL CHAR VARYING|NATIONAL CHARACTER VARYING)\s*\(\s*([0-9]+)\s*(CHAR|BYTE)?\s*\)$`)
var oracleDMCastNumber = regexp.MustCompile(`^(NUMBER|NUMERIC|DECIMAL|DEC)\s*(?:\(\s*([0-9]+)\s*(?:,\s*([+-]?[0-9]+)\s*)?\))?$`)
var oracleDMCastFloat = regexp.MustCompile(`^(FLOAT|REAL|DOUBLE PRECISION)\s*(?:\(\s*([0-9]+)\s*\))?$`)
var oracleDMCastTimestamp = regexp.MustCompile(`^TIMESTAMP\s*(?:\(\s*([0-9]+)\s*\))?( WITH (?:LOCAL )?TIME ZONE)?$`)
var oracleDMCastRaw = regexp.MustCompile(`^RAW\s*\(\s*([0-9]+)\s*\)$`)
var oracleDMCastInterval = regexp.MustCompile(`^INTERVAL (YEAR|DAY)\s*(?:\(\s*([0-9]+)\s*\))? TO (MONTH|SECOND)\s*(?:\(\s*([0-9]+)\s*\))?$`)

func oracleDMCastType(raw string, opt Options) (string, error) {
	t := strings.Join(strings.Fields(strings.ToUpper(raw)), " ")
	c := source.ColumnInfo{Name: "CAST", DataType: t}
	integer := func(raw string) int64 {
		n, _ := strconv.ParseInt(raw, 10, 64)
		return n
	}
	switch {
	case t == "DATE":
		if opt.UseDate {
			return "DATE", nil
		}
		return "TIMESTAMP(0)", nil
	case oracleDMCastChar.MatchString(t):
		m := oracleDMCastChar.FindStringSubmatch(t)
		c.DataType, c.Length = m[1], integer(m[2])
		c.DataLength, c.CharUsed = c.Length, "B"
		if m[3] == "CHAR" {
			c.CharUsed = "C"
		}
	case oracleDMCastNumber.MatchString(t):
		m := oracleDMCastNumber.FindStringSubmatch(t)
		c.DataType, c.Precision, c.Scale = m[1], integer(m[2]), integer(m[3])
		if m[2] != "" && c.Precision == 0 {
			return "", fmt.Errorf("CAST 数值精度必须在 1 到 38 之间")
		}
	case oracleDMCastFloat.MatchString(t):
		m := oracleDMCastFloat.FindStringSubmatch(t)
		c.DataType, c.Precision = m[1], integer(m[2])
		if m[2] == "" && m[1] == "REAL" {
			c.Precision = 63
		} else if m[2] == "" && m[1] == "DOUBLE PRECISION" {
			c.Precision = 126
		}
		if m[2] != "" && (c.Precision < 1 || c.Precision > 126) {
			return "", fmt.Errorf("CAST 浮点精度必须在 1 到 126 之间")
		}
	case oracleDMCastTimestamp.MatchString(t):
		m := oracleDMCastTimestamp.FindStringSubmatch(t)
		c.DataType, c.DateTimePrecision = "TIMESTAMP"+m[2], 6
		if m[1] != "" {
			c.DateTimePrecision = integer(m[1])
		}
	case oracleDMCastRaw.MatchString(t):
		m := oracleDMCastRaw.FindStringSubmatch(t)
		c.DataType, c.DataLength = "RAW", integer(m[1])
	case oracleDMCastInterval.MatchString(t):
		m := oracleDMCastInterval.FindStringSubmatch(t)
		if m[1] == "YEAR" && (m[3] != "MONTH" || m[4] != "") || m[1] == "DAY" && m[3] != "SECOND" {
			return "", fmt.Errorf("不支持 CAST 类型 %s", raw)
		}
		c.DataType, c.Precision = "INTERVAL "+m[1]+" TO "+m[3], 2
		if m[2] != "" {
			c.Precision = integer(m[2])
		}
		if m[1] == "DAY" {
			c.Scale = 6
			if m[4] != "" {
				c.Scale = integer(m[4])
			}
		}
	case t == "INT" || t == "INTEGER" || t == "SMALLINT" || t == "BINARY_FLOAT" || t == "BINARY_DOUBLE":
	default:
		return "", fmt.Errorf("不支持 Oracle CAST 类型 %s（复杂类型需原生转换器）", raw)
	}
	if err := typemap.ValidateOracleDamengColumn(c); err != nil {
		return "", err
	}
	return typemap.OracleToDameng(c, opt.CharInLength, opt.UseNvarchar2), nil
}
