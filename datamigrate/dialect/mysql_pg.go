package dialect

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

type MySQLVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

var mysqlVersionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

func ParseMySQLVersion(raw string) (MySQLVersion, error) {
	v := MySQLVersion{}
	if strings.Contains(strings.ToLower(raw), "mariadb") {
		return v, fmt.Errorf("MariaDB 未列入 MySQL 兼容认证")
	}
	m := mysqlVersionRE.FindStringSubmatch(raw)
	if m == nil {
		return v, fmt.Errorf("无法解析 MySQL 版本 %q", raw)
	}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if !(v.Major == 5 && v.Minor == 7 || v.Major == 8 && v.Minor == 0) {
		return v, fmt.Errorf("仅支持 MySQL 5.7 / 8.0，当前 %s", raw)
	}
	return v, nil
}
func (v MySQLVersion) AtLeast(major, minor, patch int) bool {
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}
func (v MySQLVersion) Expressions() bool { return v.AtLeast(8, 0, 13) }
func (v MySQLVersion) Checks() bool      { return v.AtLeast(8, 0, 16) }

// PGDefault preserves literal categories and does not pass source SQL through.
func PGDefault(raw, typ string, nullable bool, v MySQLVersion) (string, error) {
	tokens, e := sqlsyntax.Lex(raw)
	if e != nil {
		return "", e
	}
	if len(tokens) == 0 {
		return "", fmt.Errorf("默认表达式为空")
	}
	if clause, handled, err := pgCurrentTimeDefault(tokens, typ, v); handled {
		return clause, err
	}
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
	switch normalized {
	case "CURRENT_DATE", "CURRENT_DATE()":
		if typ != "DATE" || !v.Expressions() {
			return "", fmt.Errorf("CURRENT_DATE 默认值需要 MySQL 8.0.13+ DATE 列")
		}
		return " DEFAULT (CURRENT_DATE)", nil
	}
	p := sqlsyntax.Expr{Tokens: tokens, TypedLiteral: true}
	lit, e := p.Parse()
	if e != nil {
		return "", e
	}
	// No compound comparison/boolean expression is accepted as a literal default.
	if strings.HasPrefix(lit, "(") {
		return "", fmt.Errorf("仅支持常量和白名单函数默认值")
	}
	if lit == "NULL" {
		if !nullable {
			return "", fmt.Errorf("NOT NULL 列不能 DEFAULT NULL")
		}
		return " DEFAULT NULL", nil
	}
	if isNoDefaultType(strings.ToLower(typ)) {
		if !v.Expressions() {
			return "", fmt.Errorf("%s 默认值需要 MySQL 8.0.13+", typ)
		}
		return " DEFAULT (" + lit + ")", nil
	}
	return " DEFAULT " + lit, nil
}

// MySQL's automatic timestamp default must match the column precision. For a
// lower-precision source default, use an expression without narrowing the column.
func pgCurrentTimeDefault(tokens []sqlsyntax.Token, typ string, v MySQLVersion) (string, bool, error) {
	first := tokens[0]
	// Some PG-compatible catalogs deparse this keyword as a quoted function
	// call. Match the exact lowercase name and require call syntax; a bare
	// quoted identifier, a differently cased name or a qualified UDF is not it.
	quotedLocalTimestamp := first.Kind == 'q' && first.Value == "localtimestamp" && len(tokens) > 1 && tokens[1].Is("(")
	if !(first.Is("NOW") || first.Is("CURRENT_TIMESTAMP") || first.Is("LOCALTIMESTAMP") || quotedLocalTimestamp) {
		return "", false, nil
	}
	if !strings.HasPrefix(typ, "DATETIME") && !strings.HasPrefix(typ, "TIMESTAMP") {
		return "", true, fmt.Errorf("当前时间默认值只允许日期时间列")
	}
	fsp, precision := "", int64(0)
	if i := strings.Index(typ, "("); i >= 0 {
		fsp = typ[i:]
		var err error
		precision, err = strconv.ParseInt(strings.TrimSuffix(typ[i+1:], ")"), 10, 64)
		if err != nil {
			return "", true, fmt.Errorf("非法目标时间精度")
		}
	}
	switch {
	case len(tokens) == 1 && !first.Is("NOW"):
	case len(tokens) == 3 && tokens[1].Is("(") && tokens[2].Is(")"):
	case len(tokens) == 4 && !first.Is("NOW") && tokens[1].Is("(") && tokens[2].Kind == 'n' && tokens[3].Is(")"):
		n, err := strconv.ParseInt(tokens[2].Value, 10, 64)
		if err != nil || n < 0 || n > 6 {
			return "", true, fmt.Errorf("当前时间默认值精度须为 0..6")
		}
		if n > precision {
			return "", true, fmt.Errorf("当前时间默认值精度 %d 与目标列精度 %d 不一致，需明确适配", n, precision)
		}
		if n < precision {
			if !v.Expressions() {
				return "", true, fmt.Errorf("默认时间精度 %d 低于列精度 %d，需要 MySQL 8.0.13+ 表达式默认值以保留两者精度", n, precision)
			}
			return fmt.Sprintf(" DEFAULT (CAST(CURRENT_TIMESTAMP(%d) AS DATETIME(%d)))", n, precision), true, nil
		}
	default:
		return "", true, fmt.Errorf("不支持的当前时间默认表达式")
	}
	return " DEFAULT CURRENT_TIMESTAMP" + fsp, true, nil
}

// PGTableStatements consumes validated metadata. Indexes needed by AUTO_INCREMENT
// are created atomically with the column; no temporary duplicate primary key.
func (d *MySQLDialect) PGTableStatements(schema string, t source.PGTable, v MySQLVersion, name NameFunc) ([]Statement, []string, error) {
	defs := []string{}
	var auto string
	inline := []string{}
	for _, c := range t.Info.Columns {
		typ, e := typemap.PGToMySQLChecked(c)
		if e != nil {
			return nil, nil, e
		}
		line := d.QuoteIdent(name(c.Name)) + " " + typ
		if !c.IsNullable {
			line += " NOT NULL"
		}
		if c.Extra == "auto_increment" {
			if auto != "" {
				return nil, nil, fmt.Errorf("MySQL 每表只允许一个自增列")
			}
			if c.IsNullable || !(typ == "SMALLINT" || typ == "INT" || typ == "BIGINT") {
				return nil, nil, fmt.Errorf("自增列必须为非空整数")
			}
			auto = c.Name
			line += " AUTO_INCREMENT"
		} else if c.Default != nil {
			def, e := PGColumnDefault(c, typ, v)
			if e != nil {
				return nil, nil, fmt.Errorf("列 %s: %w", c.Name, e)
			}
			line += def
		}
		if c.Comment != "" {
			line += " COMMENT " + sqlsyntax.QuoteString(c.Comment)
		}
		defs = append(defs, line)
		if c.PGMySQLLongText {
			if !v.Checks() || c.Length <= 0 || typ != "LONGTEXT" {
				return nil, nil, fmt.Errorf("VARCHAR 转 LONGTEXT 需要 MySQL 8.0.16+ 执行原字符长度约束")
			}
			defs = append(defs, "CHECK ("+PGTextLengthCheck(c, name)+")")
		}
	}
	if auto != "" {
		var chosen *source.IndexInfo
		for i := range t.Indexes {
			idx := &t.Indexes[i]
			if idx.Unsupported == "" && len(idx.Columns) > 0 && idx.Columns[0] == auto && (chosen == nil || idx.IsPrimary) {
				chosen = idx
			}
		}
		if chosen == nil {
			defs = append(defs, "INDEX "+d.QuoteIdent("__migration_auto")+" ("+d.QuoteIdent(name(auto))+")")
		} else {
			cols := []string{}
			for _, c := range chosen.Columns {
				cols = append(cols, d.QuoteIdent(name(c)))
			}
			key := "INDEX " + d.QuoteIdent(name(chosen.IndexName))
			if chosen.IsPrimary {
				key = "PRIMARY KEY"
			} else if chosen.IsUnique {
				key = "UNIQUE INDEX " + d.QuoteIdent(name(chosen.IndexName))
			}
			defs = append(defs, key+" ("+strings.Join(cols, ", ")+")")
			inline = append(inline, chosen.IndexName)
		}
	}
	qn := d.QualifyTable(schema, name(t.Info.TableName))
	create := "CREATE TABLE " + qn + " (\n  " + strings.Join(defs, ",\n  ") + "\n) ENGINE=InnoDB ROW_FORMAT=DYNAMIC"
	if t.Info.Comment != "" {
		create += " COMMENT=" + sqlsyntax.QuoteString(t.Info.Comment)
	}
	return []Statement{{SQL: "DROP TABLE IF EXISTS " + qn}, {SQL: create}}, inline, nil
}

// PGTextLengthCheck preserves bounded VARCHAR semantics after external storage.
func PGTextLengthCheck(c source.ColumnInfo, name NameFunc) string {
	return fmt.Sprintf("CHAR_LENGTH(%s) <= %d", sqlsyntax.QuoteIdent(name(c.Name)), c.Length)
}

// PGColumnDefault validates the literal against the mapped column, using the
// same conversion as data copy (notably UTC temporal defaults and bytea).
func PGColumnDefault(c source.ColumnInfo, typ string, v MySQLVersion) (string, error) {
	if c.Default == nil {
		return "", nil
	}
	if strings.EqualFold(c.DataType, "bit") {
		return pgBitDefault(c)
	}
	clause, e := PGDefault(*c.Default, typ, c.IsNullable, v)
	if e != nil {
		return "", e
	}
	raw := strings.TrimPrefix(clause, " DEFAULT ")
	currentTimeExpression := strings.HasPrefix(raw, "(CAST(CURRENT_TIMESTAMP(")
	if c.DataType == "timestamptz" && (strings.HasPrefix(raw, "CURRENT_TIMESTAMP") || currentTimeExpression) {
		if !v.Expressions() {
			return "", fmt.Errorf("timestamptz 当前时间默认值需要 MySQL 8.0.13+ UTC_TIMESTAMP 表达式，避免会话时区改变存储值")
		}
		if currentTimeExpression {
			return " DEFAULT " + strings.Replace(raw, "CURRENT_TIMESTAMP", "UTC_TIMESTAMP", 1), nil
		}
		return " DEFAULT (UTC_TIMESTAMP(6))", nil
	}
	if strings.HasPrefix(raw, "CURRENT_TIMESTAMP") || currentTimeExpression || raw == "(CURRENT_DATE)" || raw == "NULL" {
		return clause, nil
	}
	if strings.HasPrefix(raw, "(") && strings.HasSuffix(raw, ")") {
		raw = raw[1 : len(raw)-1]
	}
	ts, e := sqlsyntax.Lex(raw)
	if e != nil {
		return "", e
	}
	var val any
	if len(ts) == 1 && (ts[0].Kind == 's' || ts[0].Kind == 'n') {
		val = ts[0].Value
	} else if len(ts) == 2 && (ts[0].Is("-") || ts[0].Is("+")) && ts[1].Kind == 'n' {
		val = ts[0].Text + ts[1].Text
	} else {
		return "", fmt.Errorf("默认值不是已验证常量")
	}
	if c.DataType == "bytea" {
		s := fmt.Sprint(val)
		if !strings.HasPrefix(s, `\x`) {
			return "", fmt.Errorf("bytea 默认值仅支持十六进制表示")
		}
		b, err := hex.DecodeString(s[2:])
		if err != nil {
			return "", fmt.Errorf("非法 bytea 默认值")
		}
		val = b
	}
	converted, e := valueconv.PGMySQLValue(val, c)
	if e != nil {
		return "", e
	}
	literal := ""
	switch x := converted.(type) {
	case []byte:
		literal = "X'" + hex.EncodeToString(x) + "'"
	case string:
		switch c.DataType {
		case "numeric", "decimal":
			literal = x
		default:
			literal = sqlsyntax.QuoteString(x)
		}
	default:
		literal = fmt.Sprint(x)
	}
	if isNoDefaultType(strings.ToLower(typ)) {
		return " DEFAULT (" + literal + ")", nil
	}
	return " DEFAULT " + literal, nil
}

// Bit defaults use PostgreSQL's B'...' / '...'::bit notation. Accept only
// exact-width literals; casts that pad or truncate need explicit adaptation.
func pgBitDefault(c source.ColumnInfo) (string, error) {
	if _, err := typemap.PGToMySQLChecked(c); err != nil {
		return "", err
	}
	ts, err := sqlsyntax.Lex(*c.Default)
	if err != nil {
		return "", err
	}
	for len(ts) >= 2 && ts[0].Is("(") && ts[len(ts)-1].Is(")") {
		depth, outer := 0, true
		for i, t := range ts {
			if t.Is("(") {
				depth++
			}
			if t.Is(")") {
				depth--
			}
			if depth == 0 && i != len(ts)-1 {
				outer = false
				break
			}
		}
		if !outer {
			break
		}
		ts = ts[1 : len(ts)-1]
	}
	if len(ts) > 0 && ts[0].Is("B") {
		ts = ts[1:]
	}
	if len(ts) == 0 || !(ts[0].Kind == 's' || ts[0].Is("NULL")) {
		return "", fmt.Errorf("BIT 默认值仅支持位串常量")
	}
	literal := ts[0]
	rest := ts[1:]
	if len(rest) >= 2 && rest[0].Is("::") && rest[1].Is("bit") {
		rest = rest[2:]
		if len(rest) >= 3 && rest[0].Is("(") && rest[1].Kind == 'n' && rest[2].Is(")") {
			n, e := strconv.ParseInt(rest[1].Value, 10, 64)
			if e != nil || n != c.Length {
				return "", fmt.Errorf("BIT 默认值转换长度与目标列不一致")
			}
			rest = rest[3:]
		} else if c.Length != 1 {
			// Bare ::bit means bit(1), not the column's declared length.
			return "", fmt.Errorf("BIT 默认值的 ::bit 转换长度与目标列不一致")
		}
	}
	if len(rest) != 0 {
		return "", fmt.Errorf("BIT 默认值包含不支持的表达式")
	}
	var value any
	if !literal.Is("NULL") {
		value = literal.Value
	}
	if _, err := valueconv.PGMySQLValue(value, c); err != nil {
		return "", err
	}
	if literal.Is("NULL") {
		return " DEFAULT NULL", nil
	}
	return " DEFAULT b'" + literal.Value + "'", nil
}
