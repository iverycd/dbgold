package dialect

import (
	"encoding/hex"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/valueconv"
)

func oscarDamengDefault(c source.ColumnInfo, mappedType string) (string, error) {
	tokens, err := sqlsyntax.Lex(*c.Default)
	unsupported := fmt.Errorf("默认值 %q 缺少等价达梦表达式", *c.Default)
	if err != nil || len(tokens) == 0 {
		return "", unsupported
	}
	base := source.OscarDamengColumnType(c.DataType)
	for i, token := range tokens {
		if token.Is("::") {
			// Strip only the declaration's own cast. An intermediate cast can
			// impose rounding or another conversion before the assignment.
			var cast []string
			for _, t := range tokens[i+1:] {
				if t.Kind != 'i' && t.Kind != 'q' {
					return "", unsupported
				}
				cast = append(cast, t.Text)
			}
			if source.OscarDamengColumnType(strings.Join(cast, " ")) != base {
				return "", unsupported
			}
			tokens = tokens[:i]
			break
		}
	}
	if len(tokens) == 0 {
		return "", unsupported
	}
	if len(tokens) == 1 && tokens[0].Is("NULL") {
		return " DEFAULT NULL", nil
	}
	if base == "BOOL" || base == "BOOLEAN" {
		if len(tokens) == 1 {
			switch {
			case tokens[0].Is("TRUE"), tokens[0].Kind == 's' && strings.EqualFold(tokens[0].Value, "true"):
				return " DEFAULT 1", nil
			case tokens[0].Is("FALSE"), tokens[0].Kind == 's' && strings.EqualFold(tokens[0].Value, "false"):
				return " DEFAULT 0", nil
			}
		}
		return "", unsupported
	}
	if base == "BIT" || base == "VARBIT" || base == "BIT VARYING" {
		if len(tokens) == 2 && tokens[0].Is("B") && tokens[1].Kind == 's' {
			bits, err := source.OscarDamengBitBytes(tokens[1].Value)
			if err == nil && len(bits) > 0 && int64(len(bits)) <= max(c.Length, 1) {
				return " DEFAULT 0x" + hex.EncodeToString(bits), nil
			}
		}
		return "", unsupported
	}
	if strings.HasPrefix(mappedType, "CHAR(") || strings.HasPrefix(mappedType, "VARCHAR(") || mappedType == "TEXT" || mappedType == "CLOB" {
		if base != "ARRAY" && len(tokens) == 1 && tokens[0].Kind == 's' {
			return " DEFAULT " + sqlsyntax.QuoteString(tokens[0].Value), nil
		}
		return "", unsupported
	}
	switch base {
	case "DATE", "TIME", "TIME WITH TIME ZONE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND":
		if len(tokens) == 1 {
			if tokens[0].Kind == 's' {
				converted, err := valueconv.OscarDameng(tokens[0].Value, base)
				if err != nil {
					return "", err
				}
				return " DEFAULT " + strings.Replace(valueconv.OscarDamengPlaceholder(base), "?", sqlsyntax.QuoteString(converted.(string)), 1), nil
			}
			for _, allowed := range []string{"CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "LOCALTIME", "LOCALTIMESTAMP"} {
				if tokens[0].Is(allowed) {
					return " DEFAULT " + allowed, nil
				}
			}
		}
		if len(tokens) == 3 && tokens[0].Is("NOW") && tokens[1].Is("(") && tokens[2].Is(")") {
			return " DEFAULT CURRENT_TIMESTAMP", nil
		}
		return "", unsupported
	case "BLOB", "BINARY", "VARBINARY", "XMLTYPE":
		return "", unsupported
	}
	if len(tokens) == 1 && tokens[0].Kind == 'n' {
		return " DEFAULT " + tokens[0].Text, nil
	}
	if len(tokens) == 2 && (tokens[0].Is("+") || tokens[0].Is("-")) && tokens[1].Kind == 'n' {
		return " DEFAULT " + tokens[0].Text + tokens[1].Text, nil
	}
	return "", unsupported
}

// ValidateOscarDamengForeignKey guards source referential actions before
// generating executable SQL. DM supports only nonmutating ON UPDATE actions.
func ValidateOscarDamengForeignKey(fk source.FKInfo) error {
	if fk.Unsupported != "" {
		return fmt.Errorf("%s", fk.Unsupported)
	}
	switch strings.ToUpper(strings.TrimSpace(fk.OnUpdate)) {
	case "", "NO ACTION", "RESTRICT":
	default:
		return fmt.Errorf("外键 %s 的 ON UPDATE %s 在达梦没有等价动作", fk.ConstraintName, fk.OnUpdate)
	}
	switch strings.ToUpper(strings.TrimSpace(fk.OnDelete)) {
	case "", "NO ACTION", "RESTRICT", "CASCADE", "SET NULL":
		return nil
	default:
		return fmt.Errorf("外键 %s 的 ON DELETE %s 在达梦没有等价动作", fk.ConstraintName, fk.OnDelete)
	}
}

// OscarDamengForeignKeyStatements uses DM's default NO ACTION for RESTRICT.
func OscarDamengForeignKeyStatements(d Dialect, schema string, fk source.FKInfo) []Statement {
	if ValidateOscarDamengForeignKey(fk) != nil {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(fk.OnDelete), "RESTRICT") {
		fk.OnDelete = "NO ACTION"
	}
	return d.ForeignKeyStatements(schema, fk)
}
