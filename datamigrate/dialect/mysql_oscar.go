package dialect

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
	"github.com/shopspring/decimal"
)

const oscarMySQLAutoIndex = "__migration_auto"

func (d *MySQLDialect) oscarTableStatements(schema string, info *source.TableDDLInfo, name NameFunc) ([]Statement, error) {
	if info == nil || len(info.Columns) == 0 {
		return nil, fmt.Errorf("Oscar 源表缺少列元数据")
	}
	if len(info.Columns) > 1017 {
		return nil, fmt.Errorf("表 %s 超出 InnoDB 的 1017 列上限", info.TableName)
	}
	if name == nil {
		name = func(s string) string { return s }
	}
	table := name(info.TableName)
	if !validOscarMySQLName(table) || schema != "" && !validOscarMySQLName(schema) || strings.ContainsAny(table+schema, "./\\") {
		return nil, fmt.Errorf("目标 MySQL 表名或数据库名无效: %q.%q", schema, table)
	}
	definitions := make([]string, 0, len(info.Columns)+1)
	seenColumns := make(map[string]bool, len(info.Columns))
	var auto string
	inlineBytes := int64((len(info.Columns) + 7) / 8)
	for _, col := range info.Columns {
		column := name(col.Name)
		folded := strings.ToLower(column)
		if !validOscarMySQLName(column) || seenColumns[folded] {
			return nil, fmt.Errorf("表 %s 的目标列名 %q 无效或冲突", info.TableName, column)
		}
		seenColumns[folded] = true
		typ, err := typemap.OscarToMySQLChecked(col)
		if err != nil {
			return nil, fmt.Errorf("表 %s 列 %s: %w", info.TableName, col.Name, err)
		}
		definition := "  " + d.QuoteIdent(column) + " " + typ
		if !col.IsNullable {
			definition += " NOT NULL"
		}
		if col.Extra == "auto_increment" {
			if auto != "" || col.IsNullable || !oscarMySQLIdentityType(typ) {
				return nil, fmt.Errorf("表 %s 只能迁移一个非空整数 AUTO_INCREMENT 列，收到 %s %s", info.TableName, col.Name, typ)
			}
			auto = column
			definition += " AUTO_INCREMENT"
		} else {
			if col.Identity != "" || col.IdentityInfo != nil {
				return nil, fmt.Errorf("表 %s 列 %s 的 Oscar 自增定义不能等价转换", info.TableName, col.Name)
			}
			clause, err := oscarMySQLDefault(col, typ)
			if err != nil {
				return nil, fmt.Errorf("表 %s 列 %s: %w", info.TableName, col.Name, err)
			}
			definition += clause
		}
		inlineBytes += oscarMySQLColumnBytes(col, typ)
		definitions = append(definitions, definition)
	}
	if inlineBytes > 65535 {
		return nil, fmt.Errorf("表 %s 的目标列声明超过 MySQL 65535 字节行长度上限", info.TableName)
	}
	if auto != "" {
		definitions = append(definitions, "  KEY "+d.QuoteIdent(oscarMySQLAutoIndex)+" ("+d.QuoteIdent(auto)+")")
	}
	qualified := d.QualifyTable(schema, table)
	return []Statement{
		{SQL: "DROP TABLE IF EXISTS " + qualified},
		{SQL: "CREATE TABLE " + qualified + " (\n" + strings.Join(definitions, ",\n") + "\n) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4"},
	}, nil
}

func oscarMySQLColumnBytes(col source.ColumnInfo, typ string) int64 {
	switch {
	case strings.HasPrefix(typ, "varchar("):
		return col.Length*4 + 2
	case strings.HasPrefix(typ, "char("):
		return col.Length * 4
	case strings.HasPrefix(typ, "varbinary("):
		return col.Length + 2
	case strings.HasPrefix(typ, "binary("):
		return col.Length
	case strings.HasPrefix(typ, "decimal("):
		return (col.Precision+1)/2 + 1
	case isNoDefaultType(typ):
		return 12
	case typ == "bigint" || typ == "double" || strings.HasPrefix(typ, "datetime"):
		return 8
	default:
		return 6
	}
}

func validOscarMySQLName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 64 || strings.HasSuffix(name, " ") {
		return false
	}
	for _, r := range name {
		if r == 0 || r > 0xffff {
			return false
		}
	}
	return true
}

func oscarMySQLIdentityType(typ string) bool {
	switch typ {
	case "tinyint", "smallint", "int", "bigint":
		return true
	}
	return false
}

var oscarMySQLCurrentTime = regexp.MustCompile(`(?i)^(?:CURRENT_TIMESTAMP|LOCALTIMESTAMP)(?:\(([0-6]?)\))?$|^NOW\(([0-6]?)\)$`)

func oscarMySQLDefault(col source.ColumnInfo, typ string) (string, error) {
	if col.Default == nil {
		return "", nil
	}
	raw := strings.TrimSpace(*col.Default)
	tokens, err := sqlsyntax.Lex(raw)
	if err != nil {
		return "", fmt.Errorf("Oscar 默认值 %q 无法解析: %w", raw, err)
	}
	tokens = oracleMySQLUnwrap(tokens)
	tokens = oscarMySQLUnwrapCast(tokens, col, typ)
	if len(tokens) == 1 && tokens[0].Is("NULL") {
		if !col.IsNullable {
			return "", fmt.Errorf("非空列的 DEFAULT NULL 无法保留")
		}
		return "", nil // MySQL's implicit nullable default is NULL.
	}
	literal, err := oscarMySQLDefaultLiteral(tokens, typ)
	if err != nil {
		return "", fmt.Errorf("Oscar 默认值 %q: %w", raw, err)
	}
	if literal == "" {
		return "", fmt.Errorf("Oscar 默认值 %q 在 MySQL 5.7/8.0 中无等价定义", raw)
	}
	return " DEFAULT " + literal, nil
}

func oscarMySQLUnwrapCast(tokens []sqlsyntax.Token, col source.ColumnInfo, targetType string) []sqlsyntax.Token {
	for i, token := range tokens {
		if !token.Is("::") || i == 0 || i+1 == len(tokens) {
			continue
		}
		var names []string
		for _, part := range tokens[i+1:] {
			if part.Kind != 'i' {
				return tokens
			}
			names = append(names, part.Text)
		}
		col.DataType = strings.Join(names, " ")
		mapped, err := typemap.OscarToMySQLChecked(col)
		if err == nil && mapped == targetType {
			return oracleMySQLUnwrap(tokens[:i])
		}
		break
	}
	return tokens
}

func oscarMySQLDefaultLiteral(tokens []sqlsyntax.Token, typ string) (string, error) {
	if len(tokens) == 0 || isNoDefaultType(typ) {
		return "", nil
	}
	var expression strings.Builder
	for _, token := range tokens {
		if token.Kind == 's' || token.Kind == 'q' {
			expression.WriteByte('!')
		} else {
			expression.WriteString(token.Text)
		}
	}
	if strings.HasPrefix(typ, "datetime") {
		if m := oscarMySQLCurrentTime.FindStringSubmatch(expression.String()); m != nil {
			suffix := strings.TrimPrefix(typ, "datetime")
			precision := m[1] + m[2]
			if precision != "" && suffix != "("+precision+")" && !(precision == "0" && suffix == "") {
				return "", fmt.Errorf("CURRENT_TIMESTAMP 精度与目标 %s 不匹配", typ)
			}
			return "CURRENT_TIMESTAMP" + suffix, nil
		}
	}
	numeric := len(tokens) == 1 && tokens[0].Kind == 'n' || len(tokens) == 2 && (tokens[0].Is("+") || tokens[0].Is("-")) && tokens[1].Kind == 'n'
	if numeric && oracleMySQLNumericType(typ) {
		literal := expression.String()
		if err := oracleMySQLValidateNumber(literal, typ); err != nil {
			return "", err
		}
		if typ == "double" || typ == "float" {
			return literal, nil
		}
		n, err := decimal.NewFromString(literal)
		if err != nil || typ == "tinyint(1)" && !n.IsZero() && !n.Equal(decimal.NewFromInt(1)) {
			return "", fmt.Errorf("数值字面量超出目标 %s 范围", typ)
		}
		return n.String(), nil
	}
	if len(tokens) == 1 && (tokens[0].Is("TRUE") || tokens[0].Is("FALSE")) && (typ == "tinyint(1)" || typ == "bit(1)") {
		if tokens[0].Is("TRUE") {
			return "1", nil
		}
		return "0", nil
	}
	if typ == "bit(1)" && (expression.String() == "0" || expression.String() == "1") {
		return expression.String(), nil
	}
	if len(tokens) == 2 && typ == "bit(1)" && tokens[0].Is("B") && tokens[1].Kind == 's' && (tokens[1].Value == "0" || tokens[1].Value == "1") {
		return "b'" + tokens[1].Value + "'", nil
	}
	if len(tokens) == 2 && tokens[1].Kind == 's' {
		if tokens[0].Is("X") && (strings.HasPrefix(typ, "binary(") || strings.HasPrefix(typ, "varbinary(")) {
			bytes, err := hex.DecodeString(tokens[1].Value)
			_, capacity, _ := strings.Cut(typ, "(")
			limit, _ := strconv.Atoi(strings.TrimSuffix(capacity, ")"))
			if err != nil || len(bytes) > limit {
				return "", fmt.Errorf("无效或超长的 %s 十六进制字面量", typ)
			}
			return "X'" + hex.EncodeToString(bytes) + "'", nil
		}
		if tokens[0].Is("N") && oracleMySQLCharType.MatchString(typ) || tokens[0].Is("DATE") && (typ == "date" || strings.HasPrefix(typ, "datetime")) ||
			tokens[0].Is("TIME") && strings.HasPrefix(typ, "time") || tokens[0].Is("TIMESTAMP") && strings.HasPrefix(typ, "datetime") {
			tokens = tokens[1:]
		}
	}
	if len(tokens) != 1 || tokens[0].Kind != 's' {
		return "", nil
	}
	value := tokens[0].Value
	if match := oracleMySQLCharType.FindStringSubmatch(typ); match != nil {
		length, _ := strconv.Atoi(match[2])
		if utf8.RuneCountInString(value) > length {
			return "", fmt.Errorf("字符串长度超过目标 %s", typ)
		}
		return oracleMySQLQuoteString(value), nil
	}
	if typ == "date" || strings.HasPrefix(typ, "datetime") {
		return oracleMySQLQuoteString(value), oracleMySQLValidateDate(value, typ)
	}
	if strings.HasPrefix(typ, "time") {
		if _, err := time.Parse("15:04:05", value); err != nil {
			return "", fmt.Errorf("无效时间字面量 %q", value)
		}
		precision := 0
		if strings.HasPrefix(typ, "time(") {
			precision, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(typ, "time("), ")"))
		}
		if _, fraction, ok := strings.Cut(value, "."); ok && len(strings.TrimRight(fraction, "0")) > precision {
			return "", fmt.Errorf("时间字面量精度超过目标 %s", typ)
		}
		return oracleMySQLQuoteString(value), nil
	}
	return "", nil
}
