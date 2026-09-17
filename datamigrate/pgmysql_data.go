package datamigrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

func pgKey(row []any, cols []source.ColumnInfo, names []string) ([32]byte, bool, error) {
	h := sha256.New()
	for _, name := range names {
		found := false
		for i, c := range cols {
			if c.Name != name {
				continue
			}
			found = true
			if row[i] == nil {
				return [32]byte{}, false, nil
			}
			var s string
			switch v := row[i].(type) {
			case []byte:
				s = string(v)
			default:
				s = fmt.Sprint(v)
			}
			writePGKeyPart(h, s)
		}
		if !found {
			return [32]byte{}, false, fmt.Errorf("键列 %s 不存在", name)
		}
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key, true, nil
}
func writePGKeyPart(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}
func convertPGPage(t source.PGTable, cols []string, rows [][]any) ([][]any, error) {
	if len(cols) != len(t.Info.Columns) {
		return nil, fmt.Errorf("表 %s 列数量在预检后发生变化", t.Info.TableName)
	}
	for i, c := range t.Info.Columns {
		if cols[i] != c.Name {
			return nil, fmt.Errorf("表 %s 列顺序在预检后发生变化", t.Info.TableName)
		}
	}
	out := make([][]any, len(rows))
	for j, row := range rows {
		if len(row) != len(cols) {
			return nil, fmt.Errorf("行长度与列数量不匹配")
		}
		out[j] = make([]any, len(row))
		for i, c := range t.Info.Columns {
			v, e := valueconv.PGMySQLValue(row[i], c)
			if e != nil {
				return nil, fmt.Errorf("%s.%s: %w", t.Info.TableName, c.Name, e)
			}
			if c.Extra == "auto_increment" {
				if n, ok := v.(int64); ok && n < 0 {
					return nil, fmt.Errorf("%s.%s: 自增值含负数", t.Info.TableName, c.Name)
				}
			}
			out[j][i] = v
		}
	}
	return out, nil
}
func pgPrimary(t source.PGTable) []string {
	for _, idx := range t.Indexes {
		if idx.IsPrimary {
			return idx.Columns
		}
	}
	return nil
}
func (m *Migrator) eachPGPage(ctx context.Context, t source.PGTable, targetDB *sql.DB, fn func([]string, [][]any) error) error {
	limit := m.cfg.PageSize
	if limit <= 0 {
		limit = 2000
	}
	if limit > 20000 {
		limit = 20000
	}
	for offset := int64(0); ; offset += int64(limit) {
		if e := ctx.Err(); e != nil {
			return e
		}
		var cols []string
		var rows [][]any
		var err error
		if targetDB == nil {
			cols, _, rows, err = m.reader.ReadPage(ctx, t.Info.TableName, pgPrimary(t), offset, int64(limit))
		} else {
			cols = make([]string, len(t.Info.Columns))
			for i, c := range t.Info.Columns {
				cols[i] = c.Name
			}
			queryCols := mapNames(cols, func(s string) string { return sqlsyntax.QuoteIdent(m.objName(s)) })
			order := queryCols
			queryCols = append([]string(nil), queryCols...)
			for i, c := range t.Info.Columns {
				if strings.EqualFold(c.DataType, "bit") {
					// MySQL drivers return packed bytes for BIT. Read exact-width
					// textual bits so validation and checksums match the PG reader.
					queryCols[i] = fmt.Sprintf("LPAD(BIN(CAST(%s AS UNSIGNED)), %d, '0')", queryCols[i], c.Length)
				}
			}
			rs, e := targetDB.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s.%s ORDER BY %s LIMIT %d OFFSET %d", strings.Join(queryCols, ","), sqlsyntax.QuoteIdent(m.cfg.TargetSchema), sqlsyntax.QuoteIdent(m.objName(t.Info.TableName)), strings.Join(order, ","), limit, offset))
			if e != nil {
				return e
			}
			for rs.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err = rs.Scan(ptrs...); err != nil {
					break
				}
				rows = append(rows, vals)
			}
			if err == nil {
				err = rs.Err()
			}
			rs.Close()
		}
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		converted, e := convertPGPage(t, cols, rows)
		if e != nil {
			return e
		}
		if e = fn(cols, converted); e != nil {
			return e
		}
		if len(rows) < limit {
			return nil
		}
	}
}
func (m *Migrator) pgRowCheckQuery(t source.PGTable, checksSupported bool, rules map[string]target.MySQLTextRules) (string, error) {
	if len(t.Checks) == 0 || !checksSupported {
		return "", nil
	}
	var conditions, cols []string
	for _, check := range t.Checks {
		expr, e := translatePGCheck(check.Definition, t.Info, m.objName)
		if e != nil {
			return "", e
		}
		safe, e := mysqlReadExpression(expr)
		if e != nil {
			return "", e
		}
		conditions = append(conditions, "("+safe+") IS NOT FALSE")
	}
	var parameters, textValues, textColumns []string
	for i, c := range t.Info.Columns {
		typ, e := typemap.PGToMySQLChecked(c)
		if e != nil {
			return "", e
		}
		parameter := fmt.Sprintf("input_row.p%d", i)
		parameters = append(parameters, fmt.Sprintf("? AS p%d", i))
		cast := parameter
		switch {
		case strings.HasPrefix(typ, "DECIMAL"):
			cast = "CAST(" + parameter + " AS " + typ + ")"
		case strings.HasPrefix(typ, "BIT("):
			cast = "CAST(" + parameter + " AS UNSIGNED)"
		case typ == "INT" || typ == "SMALLINT" || typ == "BIGINT" || strings.HasPrefix(typ, "TINYINT"):
			cast = "CAST(" + parameter + " AS SIGNED)"
		case typ == "DATE" || strings.HasPrefix(typ, "TIME(") || strings.HasPrefix(typ, "DATETIME("):
			cast = "CAST(" + parameter + " AS " + typ + ")"
		case typ == "FLOAT" || typ == "DOUBLE":
			cast = "(" + parameter + " + 0e0)"
		case typ == "JSON":
			cast = "CAST(" + parameter + " AS JSON)"
		case typ == "LONGBLOB":
			cast = "CAST(" + parameter + " AS BINARY)"
		default:
			r := rules[c.Name]
			if e := r.Validate(); e != nil {
				return "", e
			}
			// JSON_TABLE creates typed columns with implicit collation coercibility
			// (2). An expression with COLLATE has coercibility 0, and incorrectly
			// rejects comparisons between, for example, general_ci and bin columns.
			// CHECK is only evaluated on 8.0.16+, where JSON_TABLE is available.
			textColumns = append(textColumns, fmt.Sprintf("v%d %s CHARACTER SET %s COLLATE %s PATH '$[%d]' ERROR ON ERROR", i, typ, r.Charset, r.Collation, len(textValues)))
			textValues = append(textValues, "COALESCE(CONVERT("+parameter+" USING utf8mb4), '')")
			// Before 8.0.21 JSON_TABLE errors on JSON null; restore SQL NULL after
			// materializing an empty placeholder instead.
			cast = fmt.Sprintf("IF(%s IS NULL, NULL, text_row.v%d)", parameter, i)
		}
		cols = append(cols, cast+" AS "+sqlsyntax.QuoteIdent(m.objName(c.Name)))
	}
	from := "(SELECT " + strings.Join(parameters, ",") + ") AS input_row"
	if len(textColumns) > 0 {
		from += " JOIN JSON_TABLE(JSON_ARRAY(" + strings.Join(textValues, ",") + "), '$' COLUMNS (" + strings.Join(textColumns, ",") + ")) AS text_row ON TRUE"
	}
	return "SELECT (" + strings.Join(conditions, " AND ") + ") FROM (SELECT " + strings.Join(cols, ",") + " FROM " + from + ") AS checked_row", nil
}

// Hex-encode SQL string literals for observational expression evaluation so
// results do not depend on the inspection connection's sql_mode.
func mysqlReadExpression(expr string) (string, error) {
	ts, e := sqlsyntax.Lex(expr)
	if e != nil {
		return "", e
	}
	parts := make([]string, len(ts))
	for i, t := range ts {
		if t.Kind == 's' {
			parts[i] = "_utf8mb4 X'" + hex.EncodeToString([]byte(t.Value)) + "'"
		} else {
			parts[i] = t.Text
		}
	}
	return strings.Join(parts, " "), nil
}
