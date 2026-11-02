package dialect

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"github.com/shopspring/decimal"
)

// ErrOracleMySQLVersionRequired lets the coordinator inspect the target only
// when a table actually needs version-dependent DDL. The planner performs no I/O.
var ErrOracleMySQLVersionRequired = errors.New("Oracle → MySQL 表计划需要目标版本")

type OracleMySQLOptions struct {
	Version          MySQLVersion
	ProtectedColumns map[string]bool // Original source names, including incoming keys.
	BlockPromotion   bool            // An opaque index/expression has unknown dependencies.
}

type OracleMySQLWarning struct {
	Column string
	Reason string
}

// OracleMySQLTablePlan contains target metadata and its exact executable DDL.
// Columns retain source names; the coordinator applies target column mappings.
// Source metadata, including original defaults, is never modified.
type OracleMySQLTablePlan struct {
	Columns          []source.ColumnInfo
	Statements       []Statement
	Warnings         []OracleMySQLWarning
	PromotedColumns  []string
	DeclaredRowBytes int64
}

const oracleMySQLRowLimit int64 = 65535

// PlanOracleMySQLTable fits the complete utf8mb4 declaration, rather than only
// checking individual VARCHAR columns. Key/default-bearing columns are never
// reshaped. All validation finishes before returning DROP/CREATE statements.
func (d *MySQLDialect) PlanOracleMySQLTable(schema string, info *source.TableDDLInfo, opt OracleMySQLOptions, name NameFunc) (*OracleMySQLTablePlan, error) {
	if info == nil || len(info.Columns) == 0 {
		return nil, fmt.Errorf("Oracle → MySQL 建表缺少列元数据")
	}
	if len(info.Columns) > 1017 {
		return nil, fmt.Errorf("表 %s 有 %d 列，超过 InnoDB 的 1017 列上限", info.TableName, len(info.Columns))
	}
	if name == nil {
		name = func(s string) string { return s }
	}
	p := &OracleMySQLTablePlan{Columns: append([]source.ColumnInfo(nil), info.Columns...)}
	defaults := make([]string, len(info.Columns))
	effective := make([]bool, len(info.Columns))
	names := map[string]bool{}
	for i, c := range info.Columns {
		if c.IdentityInfo != nil {
			identity := *c.IdentityInfo
			if identity.Last != nil {
				last := *identity.Last
				identity.Last = &last
			}
			p.Columns[i].IdentityInfo = &identity
		}
		mapped := d.MapType(c, "oracle", TypeOpt{})
		// Replace the old single-column TEXT shortcut with the same guarded
		// whole-table fit used below. TEXT alone can hold only 65535 bytes.
		if oracleMySQLVarchar(c) && strings.EqualFold(mapped, "text") {
			mapped = fmt.Sprintf("varchar(%d)", max(c.Length, 1))
		}
		// MySQL column names are case-insensitive even when table names are not.
		columnKey := strings.ToLower(name(c.Name))
		if names[columnKey] {
			return nil, fmt.Errorf("表 %s 的目标列名 %s 冲突", info.TableName, name(c.Name))
		}
		names[columnKey] = true
		clause, hasDefault, warning, err := oracleMySQLDefault(c, mapped, opt.Version)
		if err != nil {
			return nil, fmt.Errorf("表 %s 列 %s: %w", info.TableName, c.Name, err)
		}
		defaults[i], effective[i] = clause, hasDefault
		p.Columns[i].DataType = mapped
		p.Columns[i].Default = nil
		if clause != "" {
			value := strings.TrimPrefix(clause, " DEFAULT ")
			p.Columns[i].Default = &value
		}
		if warning != "" {
			p.Warnings = append(p.Warnings, OracleMySQLWarning{c.Name, warning})
		}
	}
	before := oracleMySQLDeclaredRowBytes(p.Columns)
	if before > oracleMySQLRowLimit {
		var candidates []int
		if !opt.BlockPromotion {
			for i, c := range info.Columns {
				if oracleMySQLVarchar(c) && !opt.ProtectedColumns[c.Name] && !effective[i] && c.Extra == "" && oracleMySQLColumnBytes(p.Columns[i]) > 20 {
					candidates = append(candidates, i)
				}
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			return info.Columns[candidates[i]].Length > info.Columns[candidates[j]].Length
		})
		for _, i := range candidates {
			p.Columns[i].DataType = "longtext"
			// Explicit NULL on a promoted column is semantically redundant.
			p.Columns[i].Default, defaults[i] = nil, ""
			p.PromotedColumns = append(p.PromotedColumns, info.Columns[i].Name)
			if oracleMySQLDeclaredRowBytes(p.Columns) <= oracleMySQLRowLimit {
				break
			}
		}
		if oracleMySQLDeclaredRowBytes(p.Columns) > oracleMySQLRowLimit {
			return nil, fmt.Errorf("表 %s 按 utf8mb4 估算声明行宽 %d 字节，超过 MySQL 65535 字节上限；没有足够可安全转换的非索引、非外键且无有效默认值 VARCHAR 列", info.TableName, before)
		}
		if opt.Version.Major == 0 {
			return nil, ErrOracleMySQLVersionRequired
		}
	}
	p.DeclaredRowBytes = oracleMySQLDeclaredRowBytes(p.Columns)
	promoted := map[string]bool{}
	for _, column := range p.PromotedColumns {
		promoted[column] = true
	}
	var definitions []string
	auto := ""
	for i, c := range p.Columns {
		line := "  " + d.QuoteIdent(name(c.Name)) + " " + c.DataType
		if !c.IsNullable {
			line += " NOT NULL"
		}
		if c.Extra == "auto_increment" {
			if auto != "" {
				return nil, fmt.Errorf("MySQL 每表只支持一个自增列")
			}
			auto = name(c.Name)
			line += " AUTO_INCREMENT"
		} else {
			line += defaults[i]
		}
		definitions = append(definitions, line)
		if !promoted[c.Name] {
			continue
		}
		bound := max(info.Columns[i].Length, 1)
		reason := fmt.Sprintf("VARCHAR(%d) 自动转换为 LONGTEXT；utf8mb4 声明行宽估算由 %d 降至 %d 字节", bound, before, p.DeclaredRowBytes)
		if opt.Version.Checks() {
			definitions = append(definitions, fmt.Sprintf("  CHECK (CHAR_LENGTH(%s) <= %d)", d.QuoteIdent(name(c.Name)), bound))
			reason += fmt.Sprintf("，使用 CHECK 保留 %d 字符上限", bound)
		} else {
			reason += fmt.Sprintf("；目标版本不执行 CHECK，目标列放宽原 %d 字符上限，字段内容完整保留", bound)
		}
		p.Warnings = append(p.Warnings, OracleMySQLWarning{c.Name, reason})
	}
	if auto != "" {
		definitions = append(definitions, "  KEY "+d.QuoteIdent("__migration_auto")+" ("+d.QuoteIdent(auto)+")")
	}
	qn := d.QualifyTable(schema, name(info.TableName))
	p.Statements = []Statement{{SQL: "DROP TABLE IF EXISTS " + qn}, {SQL: "CREATE TABLE " + qn + " (\n" + strings.Join(definitions, ",\n") + "\n) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4"}}
	return p, nil
}

func oracleMySQLVarchar(c source.ColumnInfo) bool {
	base := strings.TrimSpace(strings.ToUpper(strings.SplitN(c.DataType, "(", 2)[0]))
	return base == "VARCHAR2" || base == "NVARCHAR2" || base == "VARCHAR"
}

var oracleMySQLCharType = regexp.MustCompile(`^(var)?char\(([0-9]+)\)$`)

func oracleMySQLColumnBytes(c source.ColumnInfo) int64 {
	typ := strings.ToLower(c.DataType)
	if m := oracleMySQLCharType.FindStringSubmatch(typ); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || n > (math.MaxInt64-2)/4 {
			return math.MaxInt64 / 1024
		}
		return n*4 + 2 // utf8mb4 capacity plus a conservative length prefix.
	}
	if isNoDefaultType(typ) {
		return 20 // Conservative SQL-layer contribution of an external value.
	}
	return 32 // More than the mapped numeric/date/time storage requirements.
}

func oracleMySQLDeclaredRowBytes(columns []source.ColumnInfo) int64 {
	n := int64(32 + (len(columns)+7)/8)
	for _, c := range columns {
		n += oracleMySQLColumnBytes(c)
	}
	return n
}

var oracleMySQLNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
var oracleMySQLNow = regexp.MustCompile(`(?i)^(SYSDATE|SYSTIMESTAMP|CURRENT_DATE|CURRENT_TIMESTAMP|LOCALTIMESTAMP)(?:\(\s*([0-6]?)\s*\))?$`)
var oracleMySQLDecimalType = regexp.MustCompile(`^decimal\(([0-9]+),([0-9]+)\)$`)

// effective is based on the source expression, even when its target clause must
// be omitted. Such columns remain ineligible for automatic VARCHAR promotion.
func oracleMySQLDefault(c source.ColumnInfo, typ string, version MySQLVersion) (clause string, effective bool, warning string, err error) {
	if c.Default == nil || strings.TrimSpace(*c.Default) == "" || c.Extra == "auto_increment" {
		return "", false, "", nil
	}
	raw := strings.TrimSpace(*c.Default)
	// Lex deliberately rejects floating overflow. Recognize a numeric literal
	// first so an invalid literal is rejected, rather than mistaken for an
	// unsupported expression whose default may be omitted.
	numericRaw := raw
	for strings.HasPrefix(numericRaw, "(") && strings.HasSuffix(numericRaw, ")") {
		numericRaw = strings.TrimSpace(numericRaw[1 : len(numericRaw)-1])
	}
	if oracleMySQLNumber.MatchString(numericRaw) && oracleMySQLNumericType(strings.ToLower(typ)) {
		if err := oracleMySQLValidateNumber(numericRaw, strings.ToLower(typ)); err != nil {
			return "", true, "", err
		}
		return " DEFAULT " + numericRaw, true, "", nil
	}
	ts, lexErr := sqlsyntax.Lex(raw)
	if lexErr != nil {
		return "", true, fmt.Sprintf("默认表达式 %q 无法安全转换，已省略默认值并保留原可空性；业务写入须显式提供值", raw), nil
	}
	ts = oracleMySQLUnwrap(ts)
	if len(ts) == 0 {
		return "", false, "", nil
	}
	emptyString := len(ts) == 1 && ts[0].Kind == 's' && ts[0].Value == "" || len(ts) == 2 && ts[0].Is("N") && ts[1].Kind == 's' && ts[1].Value == ""
	if len(ts) == 1 && ts[0].Is("NULL") || emptyString {
		if c.IsNullable && !isNoDefaultType(strings.ToLower(typ)) {
			return " DEFAULT NULL", false, "", nil
		}
		return "", false, "", nil
	}
	target := strings.ToLower(typ)
	numeric := ""
	if len(ts) == 1 && ts[0].Kind == 'n' {
		numeric = ts[0].Text
	} else if len(ts) == 2 && (ts[0].Is("+") || ts[0].Is("-")) && ts[1].Kind == 'n' {
		numeric = ts[0].Text + ts[1].Text
	} else if len(ts) == 1 && (ts[0].Is("TRUE") || ts[0].Is("FALSE")) {
		numeric = "0"
		if ts[0].Is("TRUE") {
			numeric = "1"
		}
	}
	var literal string
	stringValue, isString := "", false
	if len(ts) == 1 && ts[0].Kind == 's' {
		stringValue, isString = ts[0].Value, true
	} else if len(ts) == 2 && ts[0].Is("N") && ts[1].Kind == 's' {
		stringValue, isString = ts[1].Value, true
	} else if len(ts) == 2 && (ts[0].Is("DATE") || ts[0].Is("TIMESTAMP")) && ts[1].Kind == 's' && (strings.HasPrefix(target, "datetime") || strings.HasPrefix(target, "timestamp") || target == "date") {
		stringValue, isString = ts[1].Value, true
	}
	if isString {
		if oracleMySQLNumericType(target) {
			if err := oracleMySQLValidateNumber(strings.TrimSpace(stringValue), target); err != nil {
				return "", true, "", fmt.Errorf("数值默认值 %q 无效: %w", raw, err)
			}
			literal = strings.TrimSpace(stringValue)
		} else if strings.HasPrefix(target, "datetime") || strings.HasPrefix(target, "timestamp") || target == "date" {
			if err := oracleMySQLValidateDate(stringValue, target); err != nil {
				return "", true, "", err
			}
			literal = oracleMySQLQuoteString(stringValue)
		} else {
			if m := oracleMySQLCharType.FindStringSubmatch(target); m != nil {
				n, _ := strconv.ParseInt(m[2], 10, 64)
				if int64(utf8.RuneCountInString(stringValue)) > n {
					return "", true, "", fmt.Errorf("字符串默认值超过目标 %s 字符上限", typ)
				}
			}
			literal = oracleMySQLQuoteString(stringValue)
		}
	} else if numeric != "" {
		if oracleMySQLNumericType(target) {
			if err := oracleMySQLValidateNumber(numeric, target); err != nil {
				return "", true, "", err
			}
		} else if strings.HasPrefix(target, "datetime") || strings.HasPrefix(target, "timestamp") || target == "date" {
			return "", true, "", fmt.Errorf("日期时间列不能使用数值默认值 %q", raw)
		}
		literal = numeric
	}
	if literal != "" {
		if isNoDefaultType(target) {
			if version.Major == 0 {
				return "", true, "", ErrOracleMySQLVersionRequired
			}
			if !version.Expressions() {
				return "", true, fmt.Sprintf("默认表达式 %q 已省略：目标版本不支持 %s 默认值；保留原可空性，业务写入须显式提供值", raw, typ), nil
			}
			return " DEFAULT (" + literal + ")", true, "", nil
		}
		return " DEFAULT " + literal, true, "", nil
	}
	// Match the complete expression, preserving string/identifier categories.
	var compact strings.Builder
	for _, token := range ts {
		if token.Kind == 's' || token.Kind == 'q' {
			compact.WriteString("!")
		} else {
			compact.WriteString(token.Text)
		}
	}
	if match := oracleMySQLNow.FindStringSubmatch(compact.String()); match != nil && (strings.HasPrefix(target, "datetime") || strings.HasPrefix(target, "timestamp")) {
		fsp := ""
		if i := strings.Index(target, "("); i >= 0 {
			fsp = target[i:]
		}
		// A specified lower precision cannot be silently widened.
		if match[2] != "" && fsp != "("+match[2]+")" && !(match[2] == "0" && fsp == "") {
			return "", true, fmt.Sprintf("默认表达式 %q 的精度与目标 %s 不一致，已省略默认值并保留原可空性", raw, typ), nil
		}
		return " DEFAULT CURRENT_TIMESTAMP" + fsp, true, "", nil
	}
	return "", true, fmt.Sprintf("默认表达式 %q 不支持安全转换，已省略默认值并保留原可空性；业务写入须显式提供值", raw), nil
}

func oracleMySQLUnwrap(ts []sqlsyntax.Token) []sqlsyntax.Token {
	for len(ts) >= 2 && ts[0].Is("(") && ts[len(ts)-1].Is(")") {
		depth, whole := 0, true
		for i, token := range ts {
			if token.Is("(") {
				depth++
			} else if token.Is(")") {
				depth--
			}
			if depth < 0 || depth == 0 && i < len(ts)-1 {
				whole = false
				break
			}
		}
		if !whole || depth != 0 {
			break
		}
		ts = ts[1 : len(ts)-1]
	}
	return ts
}

func oracleMySQLNumericType(typ string) bool {
	return typ == "smallint" || typ == "int" || typ == "bigint" || strings.HasPrefix(typ, "tinyint") || strings.HasPrefix(typ, "decimal(") || typ == "double" || typ == "float"
}

func oracleMySQLValidateNumber(raw, typ string) error {
	if !oracleMySQLNumber.MatchString(raw) {
		return fmt.Errorf("目标 %s 需要数值常量，收到 %q", typ, raw)
	}
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exponent, err := strconv.ParseInt(raw[i+1:], 10, 32)
		if err != nil || exponent < -1000 || exponent > 1000 {
			return fmt.Errorf("数值默认值超出 MySQL 范围")
		}
	}
	if typ == "double" || typ == "float" {
		bits := 64
		if typ == "float" {
			bits = 32
		}
		v, err := strconv.ParseFloat(raw, bits)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return fmt.Errorf("数值默认值超出 %s 范围", typ)
		}
		if v == 0 {
			n, err := decimal.NewFromString(raw)
			if err != nil || !n.IsZero() {
				return fmt.Errorf("数值默认值超出 %s 范围", typ)
			}
		}
		return nil
	}
	n, err := decimal.NewFromString(raw)
	if err != nil {
		return fmt.Errorf("数值默认值无效: %w", err)
	}
	if m := oracleMySQLDecimalType.FindStringSubmatch(typ); m != nil {
		p, _ := strconv.Atoi(m[1])
		scale, _ := strconv.Atoi(m[2])
		if p < 1 || p > 65 || scale < 0 || scale > 30 || scale > p {
			return fmt.Errorf("非法目标类型 %s", typ)
		}
		bound := decimal.New(1, int32(p-scale))
		if !n.Equal(n.Truncate(int32(scale))) || n.Abs().GreaterThanOrEqual(bound) {
			return fmt.Errorf("数值默认值不能无损存入 %s", typ)
		}
		return nil
	}
	bits := 64
	switch {
	case typ == "smallint":
		bits = 16
	case typ == "int":
		bits = 32
	case strings.HasPrefix(typ, "tinyint"):
		bits = 8
	}
	minValue := decimal.NewFromInt(math.MinInt64)
	maxValue := decimal.NewFromInt(math.MaxInt64)
	if bits < 64 {
		minValue = decimal.NewFromInt(-(int64(1) << (bits - 1)))
		maxValue = decimal.NewFromInt((int64(1) << (bits - 1)) - 1)
	}
	if !n.Equal(n.Truncate(0)) || n.LessThan(minValue) || n.GreaterThan(maxValue) {
		return fmt.Errorf("数值默认值不能无损存入 %s", typ)
	}
	return nil
}

func oracleMySQLValidateDate(raw, typ string) error {
	layout := "2006-01-02 15:04:05.999999"
	if len(raw) == 10 {
		layout = "2006-01-02"
	}
	t, err := time.Parse(layout, raw)
	if err != nil || t.Year() < 1000 || t.Year() > 9999 || typ == "date" && len(raw) != 10 {
		return fmt.Errorf("日期默认值 %q 不能存入 %s", raw, typ)
	}
	precision := 0
	if i := strings.Index(typ, "("); i >= 0 {
		precision, _ = strconv.Atoi(strings.TrimSuffix(typ[i+1:], ")"))
	}
	if i := strings.Index(raw, "."); i >= 0 && len(raw)-i-1 > precision && strings.Trim(raw[i+1+precision:], "0") != "" {
		return fmt.Errorf("日期默认值 %q 的小数秒精度超过 %s", raw, typ)
	}
	return nil
}

func oracleMySQLQuoteString(value string) string {
	// Hex literals avoid dependence on NO_BACKSLASH_ESCAPES for Oracle strings.
	if strings.ContainsAny(value, "\\\x00\x1a\r\n") {
		return "_utf8mb4 X'" + hex.EncodeToString([]byte(value)) + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
