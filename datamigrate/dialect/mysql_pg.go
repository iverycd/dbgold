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
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
	dt := strings.HasPrefix(typ, "DATETIME") || strings.HasPrefix(typ, "TIMESTAMP")
	switch normalized {
	case "NOW()", "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP()":
		if !dt {
			return "", fmt.Errorf("CURRENT_TIMESTAMP 只允许日期时间列")
		}
		fsp := ""
		if i := strings.Index(typ, "("); i >= 0 {
			fsp = typ[i:]
		}
		return " DEFAULT CURRENT_TIMESTAMP" + fsp, nil
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

// PGColumnDefault validates the literal against the mapped column, using the
// same conversion as data copy (notably UTC temporal defaults and bytea).
func PGColumnDefault(c source.ColumnInfo, typ string, v MySQLVersion) (string, error) {
	if c.Default == nil {
		return "", nil
	}
	clause, e := PGDefault(*c.Default, typ, c.IsNullable, v)
	if e != nil {
		return "", e
	}
	raw := strings.TrimPrefix(clause, " DEFAULT ")
	if c.DataType == "timestamptz" && strings.HasPrefix(raw, "CURRENT_TIMESTAMP") {
		if !v.Expressions() {
			return "", fmt.Errorf("timestamptz 当前时间默认值需要 MySQL 8.0.13+ UTC_TIMESTAMP 表达式，避免会话时区改变存储值")
		}
		return " DEFAULT (UTC_TIMESTAMP(6))", nil
	}
	if strings.HasPrefix(raw, "CURRENT_TIMESTAMP") || raw == "(CURRENT_DATE)" || raw == "NULL" {
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
