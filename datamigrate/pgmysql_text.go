package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

func pgTextColumn(c source.ColumnInfo) bool {
	typ, err := typemap.PGToMySQLChecked(c)
	return err == nil && (strings.Contains(typ, "CHAR") || strings.Contains(typ, "TEXT"))
}

func pgTextLength(c source.ColumnInfo) int64 {
	if c.DataType == "uuid" {
		return 36
	}
	return max(c.Length, 1)
}

func (m *Migrator) checkPGMySQLTextStructure(p *PGMySQLPreflight) {
	for _, t := range p.tables {
		rules := p.textRules[t.Info.TableName]
		rowBytes := int64(0)
		minRecord := int64(32 + (len(t.Info.Columns)+7)/8)
		for _, c := range t.Info.Columns {
			typ, err := typemap.PGToMySQLChecked(c)
			if err != nil {
				continue
			}
			if pgTextColumn(c) {
				if err := rules[c.Name].Validate(); err != nil {
					p.add(tableRef(t.Info.TableName), err.Error(), false)
					continue
				}
			}
			switch {
			case strings.Contains(typ, "CHAR("):
				size := pgTextLength(c) * int64(rules[c.Name].MaxBytes)
				rowBytes += size + 2
				if strings.HasPrefix(typ, "VARCHAR") || size >= 768 {
					minRecord += 22
				} else {
					minRecord += size
				}
			case typ == "LONGTEXT" || typ == "LONGBLOB" || typ == "JSON":
				rowBytes += 20
				minRecord += 22
			default:
				rowBytes += 32
				minRecord += 32
			}
		}
		if minRecord > int64(min(p.Target.PageSize/2-200, 16000)) {
			p.add(tableRef(t.Info.TableName), "预计 InnoDB 最小行内记录超过目标页大小限制", true)
		}
		if len(t.Info.Columns) > 1017 || rowBytes > 65535 {
			p.add(tableRef(t.Info.TableName), "列数或预计行宽超过 MySQL 限制", true)
		}
		for _, idx := range t.Indexes {
			if !m.wantPGObject(idxRef(idx).Kind) && m.cfg.objectMode() {
				continue
			}
			if err := checkPGIndex(t, idx, p.Target, rules); err != nil {
				p.add(idxRef(idx), err.Error(), true)
			}
		}
	}
	for _, fk := range p.validationFKs {
		for i, c := range fk.Columns {
			a, b := p.textRules[fk.TableName][c], p.textRules[fk.RefTable][fk.RefColumns[i]]
			if a.Charset != b.Charset || a.Collation != b.Collation {
				p.add(ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}, "外键两端目标列字符集或排序规则不一致", false)
			}
		}
	}
}

// The server computes weights: Unicode/case/accent equivalence cannot be
// reproduced by lowercasing Go strings. For PAD SPACE, trim the server
// weights of trailing spaces, not characters padded to a fixed length: UCA
// expansions (e.g. ß = ss) can have different character counts but equal keys.
func pgWeightExpression(c source.ColumnInfo, r target.MySQLTextRules, expr string) string {
	typ, _ := typemap.PGToMySQLChecked(c)
	if strings.HasPrefix(typ, "CHAR(") {
		expr = "RTRIM(" + expr + ")"
	}
	if r.PadSpace {
		return "TRIM(TRAILING WEIGHT_STRING(CONVERT(_utf8mb4 X'20' USING " + r.Charset + ") COLLATE " + r.Collation + ") FROM WEIGHT_STRING(" + expr + "))"
	}
	return "WEIGHT_STRING(" + expr + ")"
}

// inspectPGTextPage is read-only. Batches are capped both by expression count
// and packet bytes. It checks round trips for ALL character values, not just keys.
// Returned rows contain server weights for textual key columns and encoded byte
// lengths for storage checks; the original data remains untouched.
func inspectPGTextPage(ctx context.Context, db *sql.DB, t source.PGTable, rules map[string]target.MySQLTextRules, rows [][]any, keyCols map[string]bool, maxPacket int) ([][]any, [][]int, error) {
	weighted := make([][]any, len(rows))
	sizes := make([][]int, len(rows))
	for i, row := range rows {
		weighted[i] = append([]any(nil), row...)
		sizes[i] = make([]int, len(row))
	}
	type cell struct {
		row, col int
		value    string
	}
	var pending []cell
	var selects []string
	var args []any
	packet := 0
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		rs, err := db.QueryContext(ctx, strings.Join(selects, " UNION ALL "), args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		seen := make([]bool, len(pending))
		count := 0
		for rs.Next() {
			var id, n int
			var back, weight []byte
			if err = rs.Scan(&id, &back, &n, &weight); err != nil {
				return err
			}
			if id < 0 || id >= len(pending) || seen[id] {
				return fmt.Errorf("目标字符校验返回无效行号")
			}
			seen[id] = true
			count++
			c := pending[id]
			if string(back) != c.value {
				return fmt.Errorf("%s.%s 含目标字符集 %s 无法无损表示的字符", t.Info.TableName, t.Info.Columns[c.col].Name, rules[t.Info.Columns[c.col].Name].Charset)
			}
			sizes[c.row][c.col] = n
			if keyCols[t.Info.Columns[c.col].Name] {
				weighted[c.row][c.col] = weight
			}
		}
		if err = rs.Err(); err != nil {
			return err
		}
		if count != len(pending) {
			return fmt.Errorf("目标字符校验返回行数不匹配")
		}
		pending = nil
		selects = nil
		args = nil
		packet = 0
		return nil
	}
	for ri, row := range rows {
		for ci, c := range t.Info.Columns {
			if row[ci] == nil || !pgTextColumn(c) {
				continue
			}
			r := rules[c.Name]
			if err := r.Validate(); err != nil {
				return nil, nil, err
			}
			var value string
			if b, ok := row[ci].([]byte); ok {
				value = string(b)
			} else {
				value = fmt.Sprint(row[ci])
			}
			estimate := len(value) + 2048
			if maxPacket > 0 && estimate >= maxPacket {
				return nil, nil, fmt.Errorf("%s.%s 字符校验超过 max_allowed_packet", t.Info.TableName, c.Name)
			}
			if len(pending) >= 64 || (maxPacket > 0 && packet+estimate >= maxPacket) {
				if err := flush(); err != nil {
					return nil, nil, err
				}
			}
			// Bind bytes explicitly as UTF-8; connection defaults cannot reinterpret them.
			converted := "CONVERT(CONVERT(? USING utf8mb4) USING " + r.Charset + ") COLLATE " + r.Collation
			weight := "NULL"
			if keyCols[c.Name] {
				weight = pgWeightExpression(c, r, "v")
			}
			selects = append(selects, fmt.Sprintf("SELECT %d,CAST(CONVERT(v USING utf8mb4) AS BINARY),OCTET_LENGTH(v),%s FROM (SELECT %s AS v) AS text_value", len(pending), weight, converted))
			args = append(args, []byte(value))
			pending = append(pending, cell{ri, ci, value})
			packet += estimate
		}
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	return weighted, sizes, nil
}

// Recheck immediately before any target mutation. Existing column overrides
// must stay stable as well as defaults used by CREATE TABLE.
func (m *Migrator) recheckPGMySQLText(ctx context.Context, p *PGMySQLPreflight, db *sql.DB) error {
	current, err := target.ReadMySQLDatabaseTextRules(ctx, db, p.Target.Database)
	if err != nil {
		return err
	}
	if current != p.Target.TextRules() {
		return fmt.Errorf("目标数据库默认字符配置在预检后变化，请重新预检")
	}
	if m.cfg.Content == "data_only" || m.cfg.objectMode() || len(m.cfg.ViewNames) > 0 {
		for _, t := range p.tables {
			cols, err := loadMySQLColumns(ctx, db, p.Target.Database, m.objName(t.Info.TableName))
			if err != nil {
				return err
			}
			actual := map[string]string{}
			for _, c := range cols {
				actual[c.name] = c.collation.String
			}
			for name, r := range p.textRules[t.Info.TableName] {
				if actual[m.objName(name)] != r.Collation {
					return fmt.Errorf("目标列 %s.%s 排序规则在预检后变化，请重新预检", t.Info.TableName, name)
				}
			}
		}
	}
	return nil
}

// Empty tables and schema-only runs still need representable string defaults.
func validatePGTextDefaults(ctx context.Context, p *PGMySQLPreflight, db *sql.DB) error {
	for _, t := range p.tables {
		row := make([]any, len(t.Info.Columns))
		for i, c := range t.Info.Columns {
			if !pgTextColumn(c) || c.Default == nil {
				continue
			}
			typ, err := typemap.PGToMySQLChecked(c)
			if err != nil {
				return err
			}
			clause, err := dialect.PGColumnDefault(c, typ, p.Target.Parsed)
			if err != nil {
				return err
			}
			// PGColumnDefault has already reduced text defaults to a quoted constant
			// (optionally parenthesized for LONGTEXT), or NULL. Do not execute source SQL.
			tokens, err := sqlsyntax.Lex(clause)
			if err != nil {
				return err
			}
			for _, token := range tokens {
				if token.Kind == 's' {
					row[i] = token.Value
				}
			}
		}
		if _, _, err := inspectPGTextPage(ctx, db, t, p.textRules[t.Info.TableName], [][]any{row}, nil, p.Target.MaxAllowedPacket); err != nil {
			return &pgObjectError{tableRef(t.Info.TableName), fmt.Errorf("字符默认值校验失败: %w", err)}
		}
	}
	return nil
}
