package dialect

import (
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/valueconv"
	"encoding/hex"
	"fmt"
	"strings"
)

// SQLServerDefaultDecimalLiteral supplies the same validated constant used
// by the target DDL to the unbounded DECIMAL shape profiler.
func SQLServerDefaultDecimalLiteral(c source.ColumnInfo, src string) (string, bool, error) {
	if c.Default == nil || c.SQLServerDeferredDefault || c.SQLServerIdentitySeed != "" {
		return "", false, nil
	}
	value, null, err := sqlServerDefaultLiteral(c, src)
	if err != nil || null {
		return "", false, err
	}
	if _, _, err := valueconv.SQLServerDecimalDigits(value); err != nil {
		return "", false, fmt.Errorf("列 %s 默认值: %w", c.Name, err)
	}
	return value, true, nil
}

func sqlServerCrossDefault(c source.ColumnInfo, src, mapped string) (string, error) {
	if c.Default == nil || c.SQLServerDeferredDefault {
		return "", nil
	}
	if current := sqlServerCurrentDefault(*c.Default, src, mapped); current != "" {
		return " DEFAULT " + current, nil
	}
	value, null, err := sqlServerDefaultLiteral(c, src)
	if err != nil {
		return "", err
	}
	if null {
		// A NULL default can exist on a NOT NULL column. Inserting without an
		// explicit value still fails, as it did at the source.
		return " DEFAULT NULL", nil
	}
	var input any = value
	if strings.HasPrefix(mapped, "VARBINARY(") {
		if raw, ok := strings.CutPrefix(value, `\x`); source.IsPGFamily(src) && ok {
			input, err = hex.DecodeString(raw)
		} else {
			err = fmt.Errorf("无法安全解析二进制默认值")
		}
		if err != nil {
			return "", fmt.Errorf("列 %s 默认值: %w", c.Name, err)
		}
	}
	converted, err := valueconv.ConvertSQLServerForSource(input, c, src)
	if err != nil {
		return "", fmt.Errorf("默认值: %w", err)
	}
	switch v := converted.(type) {
	case bool:
		if v {
			return " DEFAULT 1", nil
		}
		return " DEFAULT 0", nil
	case []byte:
		return " DEFAULT 0x" + hex.EncodeToString(v), nil
	default:
		return " DEFAULT " + SQLServerLiteral(fmt.Sprint(v)), nil
	}
}

func sqlServerDefaultLiteral(c source.ColumnInfo, src string) (string, bool, error) {
	raw := strings.TrimSpace(*c.Default)
	failed := func() (string, bool, error) {
		return "", false, fmt.Errorf("列 %s: 无法安全迁移默认值 %q", c.Name, raw)
	}
	tokens, err := sqlsyntax.Lex(raw)
	if err != nil {
		return failed()
	}
	tokens = pgOracleUnwrap(tokens)
	if (src == "oracle" || src == "dameng") && len(tokens) == 2 && (tokens[0].Is("n") || tokens[0].Is("date") || tokens[0].Is("timestamp")) && tokens[1].Kind == 's' {
		tokens = tokens[1:]
	}
	parser := sqlsyntax.Expr{Tokens: tokens, TypedLiteral: true}
	literal, err := parser.Parse()
	if err != nil {
		return failed()
	}
	tokens, err = sqlsyntax.Lex(literal)
	if err != nil {
		return failed()
	}
	if len(tokens) == 1 {
		v := tokens[0]
		switch {
		case v.Is("NULL"), src == "oracle" && v.Kind == 's' && v.Value == "":
			return "", true, nil
		case v.Kind == 'n' || v.Kind == 's' && !strings.ContainsRune(v.Value, 0):
			return v.Value, false, nil
		}
	}
	if len(tokens) == 2 && (tokens[0].Is("+") || tokens[0].Is("-")) && tokens[1].Kind == 'n' {
		return tokens[0].Text + tokens[1].Text, false, nil
	}
	return failed()
}

func sqlServerCurrentDefault(raw, src, mapped string) string {
	tokens, err := sqlsyntax.Lex(raw)
	if err != nil {
		return ""
	}
	tokens = pgOracleUnwrap(tokens)
	if source.IsPGFamily(src) && len(tokens) >= 3 && pgOracleCatalogToken(tokens[0]) && tokens[1].Is(".") {
		tokens = tokens[2:]
	}
	switch {
	case len(tokens) == 1 && tokens[0].Is("current_date") && mapped == "DATE":
		return "CONVERT(DATE,SYSDATETIME())"
	case (src == "oracle" || src == "dameng") && len(tokens) == 1 && tokens[0].Is("sysdate") && strings.HasPrefix(mapped, "DATETIME2("):
		return "CONVERT(DATETIME2(0),CONVERT(CHAR(19),SYSDATETIME(),126),126)"
	case len(tokens) == 1 && tokens[0].Is("current_timestamp"),
		(src == "oracle" || src == "dameng") && len(tokens) == 1 && tokens[0].Is("systimestamp"),
		source.IsPGFamily(src) && len(tokens) == 3 && tokens[0].Is("now") && tokens[1].Is("(") && tokens[2].Is(")"):
		if strings.HasPrefix(mapped, "DATETIMEOFFSET(") {
			return "SYSDATETIMEOFFSET()"
		}
		if strings.HasPrefix(mapped, "DATETIME2(") {
			return "SYSDATETIME()"
		}
	case len(tokens) == 1 && tokens[0].Is("localtimestamp") && strings.HasPrefix(mapped, "DATETIME2("):
		return "SYSDATETIME()"
	}
	return ""
}
