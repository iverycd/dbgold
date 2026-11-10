package dialect

import "strings"

// Classify the original literal before keyword/function conversion. DM permits
// boolean defaults on numeric columns, while PG requires a numeric expression.
// ready means the result already is SQL, including any necessary quotes.
func damengPGDefault(raw, pgType string) (def string, ready bool, err error) {
	def, literal, err := pgDefaultLiteral(raw, "达梦")
	if err != nil {
		return "", false, err
	}
	// Retain support for the legacy catalog wrapper ''value''. A valid SQL
	// literal (especially '''', representing a quote) must never be unwrapped.
	if !literal && len(def) > 4 && strings.HasPrefix(def, "''") && strings.HasSuffix(def, "''") {
		inner := def[2 : len(def)-2]
		if !strings.Contains(inner, "'") {
			def, literal = "'"+inner+"'", true
		}
	}
	value := def
	if literal {
		value = strings.ReplaceAll(def[1:len(def)-1], "''", "'")
	}
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(pgType)), "(")
	switch strings.TrimSpace(base) {
	case "smallint", "int", "integer", "bigint", "numeric", "decimal", "real", "double precision":
		switch strings.ToUpper(strings.TrimSpace(value)) {
		case "TRUE":
			return "1", true, nil
		case "FALSE":
			return "0", true, nil
		}
	}
	return def, literal, nil
}
