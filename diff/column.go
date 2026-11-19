package diff

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

type columnDefinition struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"`
	Identity  string  `json:"identity,omitempty"`
	Generated string  `json:"generated,omitempty"`
	Extra     string  `json:"extra,omitempty"`
	Charset   string  `json:"charset,omitempty"`
	Collation string  `json:"collation,omitempty"`
}

func displayColumn(c Column) columnDefinition {
	dataType := c.FormattedType
	if dataType == "" {
		dataType = c.DataType
	}
	identity := c.Identity
	if identity == "" && strings.Contains(strings.ToLower(c.Extra), "auto_increment") {
		identity = "auto_increment"
	}
	return columnDefinition{c.Name, dataType, c.IsNullable, c.Default, identity, c.Generated, c.Extra, c.Charset, c.Collation}
}

var (
	numericLiteral = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,3})?$`)
	typeArgs       = regexp.MustCompile(`^([a-z][a-z0-9 ]*)\(([0-9]+)(?:(?:,\s*)(-?[0-9]+)|( char| byte))?\)(.*)$`)
	mysqlType      = regexp.MustCompile(`^(?:tinyint|smallint|mediumint|int|integer|bigint|decimal|numeric|float|double|double precision|real|bit|char|varchar|binary|varbinary|tinytext|text|mediumtext|longtext|tinyblob|blob|mediumblob|longblob|json|date|datetime|timestamp|time|year)(?:\([0-9]+(?:,[0-9]+)?\))?(?: unsigned)?(?: zerofill)?$`)
	pgType         = regexp.MustCompile(`^(?:smallint|integer|bigint|numeric(?:\([0-9]+,-?[0-9]+\))?|real|double precision|boolean|text|character(?: varying)?(?:\([0-9]+\))?|bit(?: varying)?(?:\([0-9]+\))?|(?:time|timestamp)(?:\([0-9]+\))? (?:with|without) time zone|date|bytea|jsonb?|uuid)$`)
	dmType         = regexp.MustCompile(`^(?:tinyint|smallint|integer|bigint|number(?:\([0-9]+,-?[0-9]+\))?|real|double|char(?:\([0-9]+(?: char)?\))?|varchar(?:2)?\([0-9]+(?: char)?\)|nvarchar2\([0-9]+\)|text|clob|blob|date|(?:time|timestamp)(?:\([0-9]+\))?)$`)
	sqlWord        = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
	currentTime    = regexp.MustCompile(`(?i)^CURRENT_TIMESTAMP(?:\(([0-6]?)\))?$`)
	// Catalog defaults recognized below are individual literals, never arbitrary SQL.
	stringDefault = regexp.MustCompile(`^'((?:[^']|'')*)'(?:\s*::\s*([a-zA-Z][a-zA-Z0-9_ (),]*))?$`)
	mysqlBit      = regexp.MustCompile(`(?i)^b'([01]+)'$`)
)

func canonicalType(product, input string) string {
	// ENUM/SET may contain case-sensitive values or significant spaces.
	if strings.HasPrefix(strings.ToLower(input), "enum(") || strings.HasPrefix(strings.ToLower(input), "set(") {
		return input
	}
	f := strings.ToLower(strings.TrimSpace(input))
	base, size, scale, units, suffix := f, "", "", "", ""
	if m := typeArgs.FindStringSubmatch(f); m != nil {
		base, size, scale, units, suffix = strings.TrimSpace(m[1]), m[2], m[3], strings.TrimSpace(m[4]), m[5]
	} else if !strings.ContainsRune(f, '(') {
		for _, attr := range []string{" unsigned zerofill", " unsigned", " zerofill"} {
			if strings.HasSuffix(base, attr) {
				base, suffix = strings.TrimSuffix(base, attr), attr
				break
			}
		}
	}
	switch {
	case product == "mysql":
		if base == "integer" {
			base = "int"
		}
		switch base {
		case "tinyint", "smallint", "mediumint", "int", "bigint", "year":
			size = "" // Display width does not change the integer's range.
		case "time", "timestamp", "datetime":
			if size == "0" {
				size = ""
			}
		}
	case product == "dameng":
		switch base {
		case "int":
			base = "integer"
		case "decimal", "numeric":
			base = "number"
		case "double precision":
			base = "double"
		case "datetime":
			base = "timestamp"
		case "varchar":
			base = "varchar2"
		case "character":
			base = "char"
		}
		if units == "byte" {
			units = ""
		}
		if base == "timestamp" && size == "6" || base == "time" && size == "0" {
			size = ""
		}
	default:
		switch base {
		case "int", "int4":
			base = "integer"
		case "int2":
			base = "smallint"
		case "int8":
			base = "bigint"
		case "decimal":
			base = "numeric"
		case "varchar":
			base = "character varying"
		case "char", "bpchar":
			base = "character"
		case "bool":
			base = "boolean"
		case "float4":
			base = "real"
		case "float8":
			base = "double precision"
		case "varbit":
			base = "bit varying"
		case "timestamptz":
			base, suffix = "timestamp", " with time zone"
		case "timetz":
			base, suffix = "time", " with time zone"
		}
		if base == "time" || base == "timestamp" {
			if suffix == "" {
				suffix = " without time zone"
			}
			if size == "6" {
				size = ""
			}
		}
	}
	if size != "" {
		if scale == "" && (base == "numeric" || base == "decimal" || base == "number") {
			scale = "0"
		}
		if scale != "" {
			size += "," + scale
		}
		if units != "" {
			size += " " + units
		}
		return base + "(" + size + ")" + suffix
	}
	return base + suffix
}

func standardType(product, dataType string) bool {
	switch product {
	case "mysql":
		return mysqlType.MatchString(dataType)
	case "dameng":
		return dmType.MatchString(dataType)
	default:
		return pgType.MatchString(dataType)
	}
}

func sqlString(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func defaultForm(product string, column columnDefinition) (string, string, bool) {
	if column.Default == nil || column.Identity != "" {
		return "", "", true
	}
	value := *column.Default
	expr := strings.TrimSpace(value)
	base := strings.SplitN(canonicalType(product, column.Type), "(", 2)[0]
	numeric := regexpNumericType(base)
	temporal := strings.HasPrefix(base, "time") || base == "datetime" || base == "date"
	if temporal {
		if m := currentTime.FindStringSubmatch(expr); m != nil {
			name := "CURRENT_TIMESTAMP"
			if m[1] != "" && (m[1] != "0" || product != "mysql") {
				name += "(" + m[1] + ")"
			}
			return strings.ToLower(name), name, true
		}
		if product != "mysql" && strings.EqualFold(expr, "now()") {
			return "current_timestamp", "CURRENT_TIMESTAMP", true
		}
	}
	if product == "mysql" {
		if strings.Contains(strings.ToUpper(column.Extra), "DEFAULT_GENERATED") {
			return expr, "", false
		}
		if m := mysqlBit.FindStringSubmatch(value); m != nil && base == "bit" {
			return "bits:" + m[1], "B'" + m[1] + "'", true
		}
		if !numeric {
			return "text:" + value, sqlString(value), utf8.ValidString(value) && !strings.ContainsRune(value, 0)
		}
	} else {
		if strings.EqualFold(expr, "NULL") || strings.HasPrefix(expr, "NULL::") {
			return "", "", true
		}
		if m := stringDefault.FindStringSubmatch(expr); m != nil {
			decoded := strings.ReplaceAll(m[1], "''", "'")
			if !numeric {
				// Accept only casts back to the same column type or base type.
				cast := canonicalType(product, m[2])
				actual := canonicalType(product, column.Type)
				if m[2] != "" && cast != actual && cast != strings.SplitN(actual, "(", 2)[0] {
					return expr, "", false
				}
				return "text:" + decoded, sqlString(decoded), !strings.ContainsRune(decoded, 0)
			}
			expr = decoded
		}
		if base == "boolean" && (strings.EqualFold(expr, "true") || strings.EqualFold(expr, "false")) {
			return strings.ToLower(expr), strings.ToUpper(expr), true
		}
	}
	if numeric && numericLiteral.MatchString(expr) {
		if n, ok := new(big.Rat).SetString(expr); ok {
			return "number:" + n.RatString(), expr, true
		}
	}
	return expr, "", false
}

func regexpNumericType(base string) bool {
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "numeric", "decimal", "number", "float", "double", "double precision", "real", "year":
		return true
	}
	return strings.HasSuffix(base, " unsigned") || strings.HasSuffix(base, " zerofill")
}

type projectedColumn struct {
	definition columnDefinition
	body       string
	reason     string
}

func projectColumn(c Column, name string, from, to Catalog, options Options) projectedColumn {
	d := displayColumn(c)
	d.Name = name
	out := projectedColumn{definition: d, reason: c.Unsupported}
	crossMySQL := from.Product == "mysql" && to.Product != "mysql"
	if crossMySQL {
		mapType, ok := typemap.Get(from.Product, to.Product)
		if ok {
			d.Type = mapType(c.ColumnInfo, options.CharInLength, options.UseNvarchar2)
		}
		d.Charset, d.Collation, d.Extra = "", "", ""
		if out.reason == "" {
			out.reason = mysqlMappingIssue(c, to.Product)
		}
	} else if source.IsPGFamily(from.Product) && from.Product != to.Product && c.Collation != "" {
		out.reason = "跨产品的自定义排序规则需要人工核对"
	}

	typeName := canonicalType(to.Product, d.Type)
	if !standardType(to.Product, typeName) || source.IsPGFamily(from.Product) && c.TypeKind != "" && c.TypeKind != "b" ||
		source.IsPGFamily(from.Product) && c.TypeSchema != "" && c.TypeSchema != "pg_catalog" && c.TypeSchema != "sys_catalog" {
		out.reason = "目标类型需要人工适配"
	}
	if d.Generated != "" || strings.Contains(strings.ToLower(c.Extra), "on update") ||
		strings.Contains(strings.ToLower(c.Extra), "invisible") {
		out.reason = "生成列、隐式更新或不可见列属性需要人工处理"
	}
	if !validIdentifier(to, name) {
		out.reason = "目标列名不符合数据库限制"
	}

	_, defSQL, validDefault := defaultForm(from.Product, displayColumn(c))
	if !validDefault {
		out.reason = "默认表达式需要人工确认"
	}
	if crossMySQL && defSQL != "" {
		// Reuse recognized scalar literals and compare the converted definition.
		d.Default = &defSQL
	}
	if d.Identity != "" || validDefault && defSQL == "" {
		d.Default = nil
	}
	out.definition = d

	// Keep native MySQL declarations (including the type's signedness and
	// display attributes). All branches above checked the supported grammar.
	if to.Product == "mysql" && mysqlType.MatchString(strings.ToLower(d.Type)) {
		typeName = d.Type
	}
	body := quote(to.Product, name) + " " + typeName
	switch {
	case d.Identity == "":
	case to.Product == "mysql" && d.Identity == "auto_increment":
		body += " AUTO_INCREMENT"
	case to.Product == "dameng" && d.Identity == "auto_increment":
		body += " IDENTITY(1,1)"
	case to.Product == "postgres" && (d.Identity == "auto_increment" || d.Identity == "d"):
		body += " GENERATED BY DEFAULT AS IDENTITY"
	case to.Product == "postgres" && d.Identity == "a":
		body += " GENERATED ALWAYS AS IDENTITY"
	default:
		out.reason = "目标产品的自增列语义需要人工确认"
	}
	if to.Product == "mysql" && d.Charset != "" && sqlWord.MatchString(d.Charset) && sqlWord.MatchString(d.Collation) {
		body += " CHARACTER SET " + d.Charset + " COLLATE " + d.Collation
	} else if d.Collation != "" || d.Charset != "" {
		out.reason = "列字符集或排序规则需要人工确认"
	}
	if !d.Nullable {
		body += " NOT NULL"
	} else if to.Product == "mysql" {
		body += " NULL"
	}
	if defSQL != "" && d.Identity == "" {
		body += " DEFAULT " + defSQL
	}
	if to.Product == "mysql" && c.Comment != "" {
		if commentIssue(c.Comment, 1024) != "" {
			out.reason = commentIssue(c.Comment, 1024)
		}
		body += " COMMENT " + sqlString(c.Comment)
	}
	out.body = body
	return out
}

func mysqlMappingIssue(c Column, target string) string {
	t := strings.ToLower(c.FormattedType)
	base := strings.ToLower(c.DataType)
	if strings.Contains(t, "unsigned") || strings.Contains(t, "zerofill") {
		return "无符号或 ZEROFILL 类型需要人工核对范围"
	}
	switch base {
	case "enum", "set", "bit", "year":
		return "目标类型无法完整保留源类型的取值语义"
	case "binary", "varbinary":
		return "目标类型无法完整保留源二进制列的长度语义"
	case "decimal", "numeric":
		if target == "dameng" && c.Precision > 38 {
			return "达梦精度上限为 38"
		}
	case "char", "varchar":
		if c.Length == 0 {
			return "目标字符类型需要核对零长度定义"
		}
	case "float":
		if strings.Contains(t, "(") {
			return "浮点数精度需要人工核对"
		}
	}
	return ""
}

func columnMatches(product string, a, b columnDefinition, mappedAuto bool) bool {
	ad, _, aOK := defaultForm(product, a)
	bd, _, bOK := defaultForm(product, b)
	identity := a.Identity == b.Identity || mappedAuto && a.Identity == "auto_increment" && b.Identity == "d"
	return canonicalType(product, a.Type) == canonicalType(product, b.Type) &&
		a.Nullable == b.Nullable && identity && a.Generated == b.Generated &&
		columnExtra(a.Extra) == columnExtra(b.Extra) &&
		(a.Collation == "" || a.Collation == b.Collation) && (a.Charset == "" || a.Charset == b.Charset) &&
		ad == bd && (aOK == bOK || ad == "")
}

func columnExtra(extra string) string {
	extra = strings.ToLower(extra)
	for _, marker := range []string{"auto_increment", "default_generated"} {
		extra = strings.ReplaceAll(extra, marker, "")
	}
	return strings.TrimSpace(extra)
}

func commentIssue(value string, limit int) string {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return "注释含无效字符"
	}
	if limit > 0 && utf8.RuneCountInString(value) > limit {
		return fmt.Sprintf("注释超过目标限制（%d 字符）", limit)
	}
	return ""
}
