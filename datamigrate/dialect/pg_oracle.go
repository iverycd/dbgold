package dialect

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
	"github.com/shopspring/decimal"
	goora "github.com/sijms/go-ora/v2"
)

// PGOracleDirectSequenceDefault recognizes only a complete built-in nextval
// expression. The migration planner must independently resolve its dependency.
func PGOracleDirectSequenceDefault(raw string) bool {
	ts, err := sqlsyntax.Lex(raw)
	if err != nil {
		return false
	}
	ts = pgOracleUnwrap(ts)
	if len(ts) > 2 && pgOracleCatalogToken(ts[0]) && ts[1].Is(".") {
		ts = ts[2:]
	}
	if len(ts) < 6 || !pgOracleBuiltinToken(ts[0], "nextval") || !ts[1].Is("(") || ts[2].Kind != 's' || !ts[3].Is("::") {
		return false
	}
	tail := ts[4:]
	if len(tail) > 2 && pgOracleCatalogToken(tail[0]) && tail[1].Is(".") {
		tail = tail[2:]
	}
	return len(tail) == 2 && pgOracleBuiltinToken(tail[0], "regclass") && tail[1].Is(")")
}

func pgOracleBuiltinToken(t sqlsyntax.Token, name string) bool {
	return t.Kind == 'i' && t.Is(name) || t.Kind == 'q' && t.Value == name
}
func pgOracleCatalogToken(t sqlsyntax.Token) bool {
	return pgOracleBuiltinToken(t, "pg_catalog") || pgOracleBuiltinToken(t, "sys_catalog")
}

func pgOracleUnwrap(ts []sqlsyntax.Token) []sqlsyntax.Token {
	for len(ts) >= 2 && ts[0].Is("(") && ts[len(ts)-1].Is(")") {
		depth, whole := 0, true
		for i, t := range ts {
			if t.Is("(") {
				depth++
			}
			if t.Is(")") {
				depth--
			}
			if depth == 0 && i < len(ts)-1 {
				whole = false
				break
			}
		}
		if !whole {
			break
		}
		ts = ts[1 : len(ts)-1]
	}
	return ts
}

// PGOracleDefault returns an Oracle clause, never unvalidated source SQL.
func PGOracleDefault(c source.ColumnInfo, target string) (string, error) {
	if c.Default == nil || c.Identity != "" {
		return "", nil
	}
	if PGOracleDirectSequenceDefault(*c.Default) {
		return "", nil
	}
	ts, err := sqlsyntax.Lex(*c.Default)
	if err != nil {
		return "", err
	}
	ts = pgOracleUnwrap(ts)
	if len(ts) == 0 {
		return "", fmt.Errorf("默认值表达式为空")
	}
	if clause, handled, err := pgOracleCurrentDefault(ts, c); handled {
		return clause, err
	}
	// These casts retain textual representations. The source type validator
	// below checks the network/bit domain; no user-defined cast is erased.
	for i := 0; i+1 < len(ts); i++ {
		if ts[i].Is("::") && ts[i+1].Kind == 'i' {
			switch ts[i+1].Value {
			case "xml", "inet", "cidr", "macaddr", "macaddr8":
				if typemap.PGOracleBaseType(c.DataType) != ts[i+1].Value {
					return "", fmt.Errorf("跨文本域的默认值类型转换需要人工适配")
				}
				ts[i+1].Value = "text"
				ts[i+1].Text = "text"
			}
		}
	}
	if typemap.PGOracleBaseType(c.DataType) == "bit" || typemap.PGOracleBaseType(c.DataType) == "varbit" {
		return pgOracleBitDefault(ts, c, target)
	}
	p := sqlsyntax.Expr{Tokens: ts, TypedLiteral: true}
	lit, err := p.Parse()
	if err != nil {
		return "", err
	}
	if lit == "NULL" {
		if !c.IsNullable {
			return "", fmt.Errorf("非空列不能 DEFAULT NULL")
		}
		return " DEFAULT NULL", nil
	}
	parsed, err := sqlsyntax.Lex(lit)
	if err != nil {
		return "", err
	}
	var raw string
	if len(parsed) == 1 && (parsed[0].Kind == 's' || parsed[0].Kind == 'n') {
		raw = parsed[0].Value
	} else if len(parsed) == 2 && (parsed[0].Is("-") || parsed[0].Is("+")) && parsed[1].Kind == 'n' {
		raw = parsed[0].Text + parsed[1].Text
	} else {
		return "", fmt.Errorf("仅支持常量及白名单时间默认值")
	}
	return pgOracleConstantDefault(raw, c, target)
}

func pgOracleConstantDefault(raw string, c source.ColumnInfo, target string) (string, error) {
	if strings.ContainsRune(raw, 0) || len(raw) > 1000 {
		return "", fmt.Errorf("默认值含 NUL 或超过安全 SQL 字面量长度")
	}
	if target == "BLOB" {
		return "", fmt.Errorf("Oracle 11g 不支持自动转换 bytea 默认值")
	}
	dst := valueconv.OracleColumn{DataType: target, Nullable: c.IsNullable, Precision: c.Precision, Scale: c.Scale, CharUsed: "C"}
	if strings.HasPrefix(target, "NUMBER") {
		dst.DataType = "NUMBER"
	}
	if strings.HasPrefix(target, "TIMESTAMP") {
		dst.Scale = c.DateTimePrecision
	}
	if strings.HasPrefix(target, "VARCHAR2") {
		dst.DataType = "VARCHAR2"
	}
	if strings.HasPrefix(target, "NVARCHAR2") {
		dst.DataType = "NVARCHAR2"
	}
	if strings.HasPrefix(target, "NCHAR") {
		dst.DataType = "NCHAR"
	}
	if strings.HasPrefix(target, "CHAR") {
		dst.DataType = "CHAR"
	}
	v, _, err := valueconv.ConvertPGOracle(raw, c, dst, nil)
	if err != nil {
		return "", err
	}
	if v == nil {
		return " DEFAULT NULL", nil
	}
	switch x := v.(type) {
	case *goora.Number:
		// go-ora's String method treats a valid positive 1e-130 wire exponent
		// as zero. Render the already-validated source decimal independently.
		if n, err := decimal.NewFromString(raw); err == nil {
			return " DEFAULT " + n.String(), nil
		}
		s, err := x.String()
		return " DEFAULT " + s, err
	case goora.TimeStampTZ:
		t := time.Time(x).UTC()
		return " DEFAULT TO_TIMESTAMP_TZ(" + oracleLiteral(t.Format("2006-01-02 15:04:05.000000")+" +00:00") + ", 'YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')", nil
	case goora.TimeStamp:
		t := time.Time(x)
		if typemap.PGOracleBaseType(c.DataType) == "date" {
			return " DEFAULT DATE " + oracleLiteral(t.Format("2006-01-02")), nil
		}
		return " DEFAULT TIMESTAMP " + oracleLiteral(t.Format("2006-01-02 15:04:05.000000")), nil
	case goora.Clob:
		if x.String == "" {
			return " DEFAULT EMPTY_CLOB()", nil
		}
		return " DEFAULT " + oracleLiteral(x.String), nil
	case goora.NClob:
		if x.String == "" {
			return " DEFAULT TO_NCLOB(EMPTY_CLOB())", nil
		}
		return " DEFAULT TO_NCLOB(" + pgOracleNationalLiteral(x.String) + ")", nil
	default:
		if target == "BINARY_FLOAT" || target == "BINARY_DOUBLE" {
			suffix := "D"
			if target == "BINARY_FLOAT" {
				suffix = "F"
			}
			return " DEFAULT " + fmt.Sprint(v) + suffix, nil
		}
		if strings.HasPrefix(target, "NCHAR") || strings.HasPrefix(target, "NVARCHAR2") {
			return " DEFAULT " + pgOracleNationalLiteral(raw), nil
		}
		return " DEFAULT " + oracleLiteral(raw), nil
	}
}

// Keep national defaults ASCII on the SQL wire even when the database character
// set cannot encode their content. UNISTR counts UTF-16 surrogate pairs.
func pgOracleNationalLiteral(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r == '\\':
			b.WriteString(`\005C`)
		case r < 128:
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\%04X`, r)
		default:
			a, z := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\%04X\%04X`, a, z)
		}
	}
	return "UNISTR(" + oracleLiteral(b.String()) + ")"
}

func pgOracleBitDefault(ts []sqlsyntax.Token, c source.ColumnInfo, target string) (string, error) {
	if len(ts) > 0 && ts[0].Is("NULL") {
		if !c.IsNullable {
			return "", fmt.Errorf("非空列不能 DEFAULT NULL")
		}
		if len(ts) == 1 || len(ts) == 3 && ts[1].Is("::") && (ts[2].Is("bit") || ts[2].Is("varbit")) {
			return " DEFAULT NULL", nil
		}
		return "", fmt.Errorf("NULL 位串默认值类型转换不支持")
	}
	if len(ts) > 1 && ts[0].Is("B") {
		ts = ts[1:]
	}
	if len(ts) == 0 || ts[0].Kind != 's' {
		return "", fmt.Errorf("位串默认值必须为字符串常量")
	}
	raw := ts[0].Value
	ts = ts[1:]
	for len(ts) > 0 {
		if len(ts) < 2 || !ts[0].Is("::") || !(ts[1].Is("bit") || ts[1].Is("varbit")) {
			return "", fmt.Errorf("位串默认表达式不支持")
		}
		fixed := ts[1].Is("bit")
		ts = ts[2:]
		width := int64(1)
		if len(ts) >= 3 && ts[0].Is("(") && ts[1].Kind == 'n' && ts[2].Is(")") {
			var err error
			width, err = strconv.ParseInt(ts[1].Value, 10, 64)
			if err != nil {
				return "", fmt.Errorf("位串转换长度无效")
			}
			ts = ts[3:]
		} else if !fixed {
			width = 0
		}
		if fixed && int64(len(raw)) != width || !fixed && width > 0 && int64(len(raw)) > width {
			return "", fmt.Errorf("位串转换会改变长度")
		}
	}
	return pgOracleConstantDefault(raw, c, target)
}

func pgOracleCurrentDefault(ts []sqlsyntax.Token, c source.ColumnInfo) (string, bool, error) {
	if len(ts) > 2 && pgOracleCatalogToken(ts[0]) && ts[1].Is(".") {
		ts = ts[2:]
	}
	first := ts[0]
	if first.Kind != 'i' || !(first.Is("now") || first.Is("current_timestamp") || first.Is("localtimestamp") || first.Is("current_date")) {
		return "", false, nil
	}
	dt := typemap.PGOracleBaseType(c.DataType)
	if first.Is("current_date") {
		if len(ts) != 1 || dt != "date" {
			return "", true, fmt.Errorf("CURRENT_DATE 仅支持 DATE 列")
		}
		return " DEFAULT TRUNC(CURRENT_DATE)", true, nil
	}
	if dt != "timestamp" && dt != "timestamptz" {
		return "", true, fmt.Errorf("当前时间默认值仅支持时间戳列")
	}
	precision := c.DateTimePrecision
	switch {
	case len(ts) == 1 && !first.Is("now"):
	case len(ts) == 3 && ts[1].Is("(") && ts[2].Is(")"):
	case len(ts) == 4 && !first.Is("now") && ts[1].Is("(") && ts[2].Kind == 'n' && ts[3].Is(")"):
		n, err := strconv.ParseInt(ts[2].Value, 10, 64)
		if err != nil || n < 0 || n > 6 || n > precision {
			return "", true, fmt.Errorf("当前时间默认值精度与列精度不兼容")
		}
		precision = n
	default:
		return "", true, fmt.Errorf("当前时间默认表达式不支持")
	}
	if first.Is("localtimestamp") && dt == "timestamptz" {
		return "", true, fmt.Errorf("LOCALTIMESTAMP 到有时区列需要人工适配")
	}
	fn := "CURRENT_TIMESTAMP"
	if first.Is("localtimestamp") {
		fn = "LOCALTIMESTAMP"
	}
	clause := fmt.Sprintf("%s(%d)", fn, precision)
	if dt == "timestamp" && fn == "CURRENT_TIMESTAMP" {
		clause = fmt.Sprintf("CAST(%s AS TIMESTAMP(%d))", clause, c.DateTimePrecision)
	}
	return " DEFAULT " + clause, true, nil
}

func PGOracleDefaultWarnings(c source.ColumnInfo) []string {
	if c.Default == nil {
		return nil
	}
	ts, err := sqlsyntax.Lex(*c.Default)
	if err != nil {
		return nil
	}
	ts = pgOracleUnwrap(ts)
	if len(ts) == 0 {
		return nil
	}
	if _, handled, err := pgOracleCurrentDefault(ts, c); handled && err == nil {
		return []string{"时间默认值采用 Oracle 当前时间/会话时区语义，与 PG 事务开始时间语义不同"}
	}
	dt := typemap.PGOracleBaseType(c.DataType)
	if c.IsNullable && (dt == "bpchar" || dt == "varchar" && c.Length > 0) {
		p := sqlsyntax.Expr{Tokens: ts, TypedLiteral: true}
		if literal, err := p.Parse(); err == nil && literal == "''" {
			return []string{"空字符串默认值转 NULL"}
		}
	}
	return nil
}

func (d *OracleDialect) pgCreateTableStatements(schema string, info *source.TableDDLInfo, opt TypeOpt, name NameFunc) ([]Statement, error) {
	if info == nil || len(info.Columns) == 0 {
		return nil, fmt.Errorf("PG 表缺少列元数据")
	}
	if err := ValidatePGOracleName(name(info.TableName)); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	defs := []string{}
	for _, c := range info.Columns {
		n := name(c.Name)
		if err := ValidatePGOracleName(n); err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, fmt.Errorf("表 %s 的列名转大写后冲突: %s", info.TableName, n)
		}
		seen[n] = true
		t, err := typemap.PGToOracleChecked(c, opt.UseNvarchar2, d.CharBytes, d.NCharBytes)
		if err != nil {
			return nil, err
		}
		def, err := PGOracleDefault(c, t)
		if err != nil {
			return nil, fmt.Errorf("表 %s 列 %s: %w", info.TableName, c.Name, err)
		}
		q := d.QuoteIdent(n)
		line := q + " " + t + def
		if !c.IsNullable {
			line += " NOT NULL"
		}
		switch typemap.PGOracleBaseType(c.DataType) {
		case "int2":
			line += " CHECK (" + q + " BETWEEN -32768 AND 32767)"
		case "int4":
			line += " CHECK (" + q + " BETWEEN -2147483648 AND 2147483647)"
		case "int8":
			line += " CHECK (" + q + " BETWEEN -9223372036854775808 AND 9223372036854775807)"
		case "bool":
			line += " CHECK (" + q + " IN (0, 1))"
		case "varchar":
			if opt.UseNvarchar2 && c.Length > 0 {
				line += " CONSTRAINT " + d.QuoteIdent(PGOracleLengthCheckName(name(info.TableName), n)) + fmt.Sprintf(" CHECK (LENGTH4(%s) <= %d)", q, c.Length)
			}
		}
		defs = append(defs, line)
	}
	qn := d.QualifyTable(schema, name(info.TableName))
	return []Statement{{SQL: "DROP TABLE " + qn + " CASCADE CONSTRAINTS"}, {SQL: "CREATE TABLE " + qn + " (\n  " + strings.Join(defs, ",\n  ") + "\n)"}}, nil
}

func PGOracleLengthCheckName(table, column string) string {
	return OracleObjectName("CK", table, column, "PGLEN")
}
