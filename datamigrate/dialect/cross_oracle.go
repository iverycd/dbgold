package dialect

import (
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/valueconv"
	"fmt"
	"strings"
	"time"
)

func OracleDefaultForSource(c source.ColumnInfo, src, target string) (string, error) {
	if src == "mysql" || src == "" {
		return oracleDefault(c, target)
	}
	if c.Default == nil || c.SQLServerDeferredDefault || strings.Contains(c.Extra, "auto_increment") {
		return "", nil
	}
	tokens, err := sqlsyntax.Lex(strings.TrimSpace(*c.Default))
	if err != nil {
		return "", err
	}
	for len(tokens) > 2 && tokens[0].Is("(") && tokens[len(tokens)-1].Is(")") {
		depth, end := 0, -1
		for i, t := range tokens {
			if t.Is("(") {
				depth++
			}
			if t.Is(")") {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end != len(tokens)-1 {
			break
		}
		tokens = tokens[1 : len(tokens)-1]
	}
	if len(tokens) == 2 && tokens[0].Is("N") && tokens[1].Kind == 's' {
		tokens = tokens[1:]
	}
	bad := func() (string, error) {
		return "", fmt.Errorf("列 %s: 不支持 %s 默认表达式 %q", c.Name, src, *c.Default)
	}
	if len(tokens) == 1 {
		t := tokens[0]
		if t.Is("NULL") {
			return " DEFAULT NULL", nil
		}
		if t.Kind == 's' {
			if t.Value == "" {
				if !c.IsNullable {
					return bad()
				}
				return " DEFAULT NULL", nil
			}
			if strings.HasPrefix(target, "TIMESTAMP") {
				zone := strings.Contains(target, "TIME ZONE")
				v := strings.Replace(t.Value, "T", " ", 1)
				layout := "2006-01-02 15:04:05.999999999"
				if zone {
					layout += "Z07:00"
					v = strings.ReplaceAll(v, " +", "+")
					v = strings.ReplaceAll(v, " -", "-")
				}
				tm, e := time.Parse(layout, v)
				if e != nil {
					return bad()
				}
				if zone {
					return " DEFAULT TO_TIMESTAMP_TZ(" + oracleLiteral(tm.Format("2006-01-02 15:04:05.000000000 -07:00")) + ", 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')", nil
				}
				return " DEFAULT TIMESTAMP " + oracleLiteral(tm.Format("2006-01-02 15:04:05.999999999")), nil
			}
			if target == "DATE" {
				if _, e := time.Parse("2006-01-02", t.Value); e != nil {
					return bad()
				}
				return " DEFAULT DATE " + oracleLiteral(t.Value), nil
			}
			if strings.HasPrefix(target, "NUMBER") {
				if !oracleNumericLiteral.MatchString(t.Value) {
					return bad()
				}
				if _, e := valueconv.PGOracleNumber(t.Value, c.Precision, c.Scale); e != nil {
					return "", e
				}
				return " DEFAULT " + t.Value, nil
			}
			if target == "BLOB" || strings.HasPrefix(target, "RAW") {
				return bad()
			}
			return " DEFAULT " + oracleLiteral(t.Value), nil
		}
	}
	parts := []string{}
	for _, t := range tokens {
		parts = append(parts, t.Text)
	}
	v := strings.Join(parts, "")
	if oracleNumericLiteral.MatchString(v) {
		if strings.HasPrefix(target, "NUMBER") {
			if _, e := valueconv.PGOracleNumber(v, c.Precision, c.Scale); e != nil {
				return "", e
			}
			return " DEFAULT " + v, nil
		}
		if target == "BINARY_FLOAT" || target == "BINARY_DOUBLE" {
			return " DEFAULT " + v, nil
		}
		return bad()
	}
	upper := strings.ToUpper(v)
	if target == "DATE" {
		switch upper {
		case "CURRENT_DATE", "SYSDATE", "GETDATE()", "CURRENT_TIMESTAMP":
			return " DEFAULT SYSDATE", nil
		}
	}
	if strings.HasPrefix(target, "TIMESTAMP") {
		precision := "6"
		if a := strings.IndexByte(target, '('); a >= 0 {
			if b := strings.IndexByte(target, ')'); b > a {
				precision = target[a+1 : b]
			}
		}
		switch upper {
		case "CURRENT_TIMESTAMP", "SYSTIMESTAMP", "SYSDATETIMEOFFSET()":
			return " DEFAULT CURRENT_TIMESTAMP(" + precision + ")", nil
		case "SYSDATE", "GETDATE()", "SYSDATETIME()", "LOCALTIMESTAMP":
			return " DEFAULT LOCALTIMESTAMP(" + precision + ")", nil
		}
	}
	return bad()
}
