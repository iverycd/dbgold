package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
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

// Match only the generated length bound. Never erase arbitrary parentheses or
// accept a weaker, disabled, OR-combined, byte-length or different-column check.
func matchesPGTextLengthCheck(raw string, c source.ColumnInfo, name func(string) string) bool {
	ts, err := sqlsyntax.Lex(raw)
	if err != nil {
		return false
	}
	for len(ts) > 2 && ts[0].Is("(") && ts[len(ts)-1].Is(")") {
		depth, wraps := 0, true
		for i, tok := range ts {
			if tok.Is("(") {
				depth++
			}
			if tok.Is(")") {
				depth--
			}
			if depth == 0 && i < len(ts)-1 {
				wraps = false
				break
			}
		}
		if !wraps {
			break
		}
		ts = ts[1 : len(ts)-1]
	}
	if len(ts) != 6 || !ts[0].Is("CHAR_LENGTH") || !ts[1].Is("(") ||
		(ts[2].Kind != 'q' && ts[2].Kind != 'i') || ts[2].Value != name(c.Name) ||
		!ts[3].Is(")") || !ts[4].Is("<=") || ts[5].Kind != 'n' {
		return false
	}
	n, err := strconv.ParseInt(ts[5].Value, 10, 64)
	return err == nil && n == c.Length
}

// A conservative SQL-layer row estimate, distinct from InnoDB's page record.
// Include nullable-column bookkeeping even when all columns are NOT NULL.
func pgDeclaredRowBytes(cols []source.ColumnInfo, rules map[string]target.MySQLTextRules) int64 {
	n := int64((len(cols) + 7) / 8)
	for _, c := range cols {
		typ, err := typemap.PGToMySQLChecked(c)
		if err != nil {
			continue
		}
		switch {
		case strings.Contains(typ, "CHAR("):
			n += pgTextLength(c)*int64(rules[c.Name].MaxBytes) + 2
		case typ == "LONGTEXT" || typ == "LONGBLOB" || typ == "JSON":
			n += 20
		default:
			n += 32
		}
	}
	return n
}

// Only new tables are reshaped. Existing tables use their inspected column
// definitions. Keep key columns intact, including incoming and excluded keys.
func (m *Migrator) fitPGMySQLRow(p *PGMySQLPreflight, t *source.PGTable) {
	if !p.Target.Parsed.Checks() || len(t.Info.Columns) > 1017 || p.Target.TextRules().Validate() != nil {
		return
	}
	rules := map[string]target.MySQLTextRules{}
	for _, c := range t.Info.Columns {
		rules[c.Name] = p.Target.TextRules()
	}
	before := pgDeclaredRowBytes(t.Info.Columns, rules)
	if before <= 65535 {
		return
	}
	protected := map[string]bool{}
	for _, idx := range t.Indexes {
		// An opaque expression index cannot safely identify all its dependencies.
		if idx.Unsupported != "" || len(idx.Columns) == 0 {
			return
		}
		for _, c := range idx.Columns {
			protected[c] = true
		}
	}
	for _, table := range p.snapshot.Tables {
		for _, fk := range table.ForeignKeys {
			if fk.TableName == t.Info.TableName {
				for _, c := range fk.Columns {
					protected[c] = true
				}
			}
			if fk.RefTable == t.Info.TableName && fk.RefSchema == p.Source.Schema {
				for _, c := range fk.RefColumns {
					protected[c] = true
				}
			}
		}
	}
	var candidates []int
	for i, c := range t.Info.Columns {
		typ, err := typemap.PGToMySQLChecked(c)
		if err != nil || !strings.HasPrefix(typ, "VARCHAR(") || c.Length < 256 || protected[c.Name] || c.Extra != "" {
			continue
		}
		mapped := c
		mapped.PGMySQLLongText = true
		if _, err := dialect.PGColumnDefault(mapped, "LONGTEXT", p.Target.Parsed); err == nil {
			candidates = append(candidates, i)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return t.Info.Columns[candidates[i]].Length > t.Info.Columns[candidates[j]].Length
	})
	cols := append([]source.ColumnInfo(nil), t.Info.Columns...)
	var changed []int
	for _, i := range candidates {
		cols[i].PGMySQLLongText = true
		changed = append(changed, i)
		if pgDeclaredRowBytes(cols, rules) <= 65535 {
			break
		}
	}
	if pgDeclaredRowBytes(cols, rules) > 65535 {
		return // Keep the original plan and blocking diagnostic if no safe fit exists.
	}
	t.Info.Columns = cols
	for _, i := range changed {
		c := cols[i]
		p.Issues = append(p.Issues, CompatibilityIssue{
			Object: tableRef(t.Info.TableName), Severity: "warning",
			Reason:     fmt.Sprintf("列 %s: VARCHAR(%d) 将映射为 LONGTEXT，并用 CHECK 保留 %d 字符上限；声明行宽估算由 %d 降至 %d 字节", c.Name, c.Length, c.Length, before, pgDeclaredRowBytes(cols, rules)),
			Suggestion: "完整保留字段内容；请查看计划 DDL。LONGTEXT 的排序、分组及客户端类型行为需在业务回归中验证",
		})
	}
}

func (m *Migrator) checkPGMySQLTextStructure(p *PGMySQLPreflight) {
	for _, t := range p.tables {
		rules := p.textRules[t.Info.TableName]
		rowBytes := pgDeclaredRowBytes(t.Info.Columns, rules)
		type columnSize struct {
			name  string
			bytes int64
		}
		var wideColumns []columnSize
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
				wideColumns = append(wideColumns, columnSize{c.Name, size + 2})
				if strings.HasPrefix(typ, "VARCHAR") || size >= 768 {
					minRecord += 22
				} else {
					minRecord += size
				}
			case typ == "LONGTEXT" || typ == "LONGBLOB" || typ == "JSON":
				minRecord += 22
			default:
				minRecord += 32
			}
		}
		if minRecord > int64(min(p.Target.PageSize/2-200, 16000)) {
			p.add(tableRef(t.Info.TableName), "预计 InnoDB 最小行内记录超过目标页大小限制", true)
		}
		if len(t.Info.Columns) > 1017 {
			p.add(tableRef(t.Info.TableName), fmt.Sprintf("列数 %d 超过 InnoDB 的 1017 列上限", len(t.Info.Columns)), true)
		}
		if rowBytes > 65535 {
			sort.SliceStable(wideColumns, func(i, j int) bool { return wideColumns[i].bytes > wideColumns[j].bytes })
			var details []string
			for _, c := range wideColumns[:min(len(wideColumns), 5)] {
				details = append(details, fmt.Sprintf("%s ≈ %d 字节", c.name, c.bytes))
			}
			reason := fmt.Sprintf("按目标字符集估算的声明行宽 %d 字节，超过 MySQL 65535 字节上限（共 %d 列）", rowBytes, len(t.Info.Columns))
			if len(details) > 0 {
				reason += "；较宽字段：" + strings.Join(details, "、")
			}
			p.add(tableRef(t.Info.TableName), reason, true)
			p.Issues[len(p.Issues)-1].Suggestion = "核对列定义及索引；可按业务缩减 VARCHAR/CHAR 长度，或将合适的非索引长字段映射为 TEXT。现有数据较短不能消除建表行宽限制"
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
