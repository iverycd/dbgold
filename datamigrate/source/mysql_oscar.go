package source

import (
	"regexp"
	"strings"
)

const mysqlOscarViewSchemaPrefix = "(?:(?:`(?:``|[^`])+`|\"(?:\"\"|[^\"])+\"|[\\p{L}_][\\p{L}\\p{N}_$#]*)\\s*\\.\\s*)?"

var mysqlOscarBooleanFixes = map[string]struct {
	pattern *regexp.Regexp
	value   string
}{
	"view_portal_myitem": {
		regexp.MustCompile("(?i)(\\(\\s*" + mysqlOscarViewSchemaPrefix + "(?:`portal_item`|\"portal_item\"|portal_item)\\s*\\.\\s*(?:`DISABLED`|\"DISABLED\"|DISABLED)\\s*=\\s*)0(\\s*\\))"), "0",
	},
	"view_cns_jxkh_rule_item": {
		regexp.MustCompile("(?i)(\\(\\s*" + mysqlOscarViewSchemaPrefix + "(?:`b`|\"b\"|b)\\s*\\.\\s*(?:`ISUSED`|\"ISUSED\"|ISUSED)\\s*=\\s*)1(\\s*\\))"), "1",
	},
}

// Oscar must receive intact identifiers: removing MySQL's backticks first loses
// the boundary between a dot inside a database name and a qualified reference.
func transformMySQLViewForOscar(name, definition string) string {
	definition = transformViewDef(stripMySQLUTF8MB4Convert(definition))
	if fix, ok := mysqlOscarBooleanFixes[name]; ok {
		definition = fix.pattern.ReplaceAllString(definition, "${1}'"+fix.value+"'${2}")
	}
	return definition
}

// Preserve ordinary CONVERT(expr, type) calls for transformViewDef. Only remove
// the charset wrapper that the legacy MySQL view query also removes.
func stripMySQLUTF8MB4Convert(sql string) string {
	var out strings.Builder
	for i := 0; i < len(sql); {
		if end := mysqlViewQuotedEnd(sql, i); end > i {
			out.WriteString(sql[i:end])
			i = end
			continue
		}
		end := i
		for end < len(sql) && isWordChar(sql[end]) {
			end++
		}
		if end > i {
			if strings.EqualFold(sql[i:end], "convert") {
				open := end
				for open < len(sql) && strings.ContainsRune(" \t\r\n", rune(sql[open])) {
					open++
				}
				if open < len(sql) && sql[open] == '(' {
					if close, using := mysqlViewCharsetConvertEnd(sql, open); using >= 0 {
						out.WriteString(stripMySQLUTF8MB4Convert(sql[open+1 : using]))
						i = close + 1
						continue
					}
				}
			}
			out.WriteString(sql[i:end])
			i = end
			continue
		}
		out.WriteByte(sql[i])
		i++
	}
	return out.String()
}

func mysqlViewCharsetConvertEnd(sql string, open int) (int, int) {
	depth, using := 0, -1
	for i := open; i < len(sql); i++ {
		if end := mysqlViewQuotedEnd(sql, i); end > i {
			i = end - 1
			continue
		}
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				if using >= 0 && strings.EqualFold(strings.TrimSpace(sql[using+len("using"):i]), "utf8mb4") {
					return i, using
				}
				return -1, -1
			}
		default:
			if depth == 1 && isWordChar(sql[i]) {
				end := i + 1
				for end < len(sql) && isWordChar(sql[end]) {
					end++
				}
				if strings.EqualFold(sql[i:end], "using") {
					using = i
				}
				i = end - 1
			}
		}
	}
	return -1, -1
}

// Skip quoted identifiers, literals and comments while inspecting CONVERT.
func mysqlViewQuotedEnd(sql string, start int) int {
	if strings.HasPrefix(sql[start:], "--") {
		if end := strings.IndexByte(sql[start:], '\n'); end >= 0 {
			return start + end
		}
		return len(sql)
	}
	if strings.HasPrefix(sql[start:], "/*") {
		if end := strings.Index(sql[start+2:], "*/"); end >= 0 {
			return start + 2 + end + 2
		}
		return len(sql)
	}
	quote := sql[start]
	if quote != '\'' && quote != '"' && quote != '`' {
		return start
	}
	for i := start + 1; i < len(sql); i++ {
		if sql[i] == '\\' && quote != '`' && i+1 < len(sql) {
			i++
			continue
		}
		if sql[i] == quote {
			if i+1 < len(sql) && sql[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(sql)
}
