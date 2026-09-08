package datamigrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

type pgKeySet map[[32]byte]struct{}

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
			// utf8mb4_bin uses PAD SPACE for CHAR/VARCHAR comparisons. Trailing ASCII
			// spaces must compare equal when testing primary/unique/foreign keys.
			switch c.DataType {
			case "varchar", "bpchar", "char", "text":
				s = strings.TrimRight(s, " ")
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
func (m *Migrator) validatePGMySQLData(ctx context.Context, p *PGMySQLPreflight, db *sql.DB, readTarget bool) error {
	sets := map[ObjectRef]pgKeySet{}
	tableMap := map[string]source.PGTable{}
	p.rowCounts = map[string]int64{}
	// Full scan, not a sample. Key hashes are length-delimited; NULL-containing
	// unique keys are exempt in both PG's normal semantics and MySQL.
	for _, t := range p.tables {
		tableMap[t.Info.TableName] = t
		for _, idx := range t.Indexes {
			if idx.IsUnique && idx.Unsupported == "" {
				sets[idxRef(idx)] = pgKeySet{}
			}
		}
		var inputDB *sql.DB
		if readTarget {
			inputDB = db
		}
		checkQuery, checkErr := m.pgRowCheckQuery(t, p.Target.Parsed.Checks())
		if checkErr != nil {
			return checkErr
		}
		var checkStmt *sql.Stmt
		if checkQuery != "" {
			checkStmt, checkErr = db.PrepareContext(ctx, checkQuery)
			if checkErr != nil {
				return checkErr
			}
		}
		e := m.eachPGPage(ctx, t, inputDB, func(_ []string, rows [][]any) error {
			for _, row := range rows {
				packetBytes := 1024 + len(row)*16
				for _, v := range row {
					if b, ok := v.([]byte); ok {
						packetBytes += len(b)
					} else if v != nil {
						packetBytes += len(fmt.Sprint(v))
					}
				}
				if p.Target.MaxAllowedPacket > 0 && packetBytes > p.Target.MaxAllowedPacket {
					return fmt.Errorf("%s 行数据超过 max_allowed_packet", t.Info.TableName)
				}
				if e := checkPGRowStorage(t, row, p.Target.PageSize); e != nil {
					return e
				}
				if checkStmt != nil {
					var valid int
					if e := checkStmt.QueryRowContext(ctx, row...).Scan(&valid); e != nil {
						return fmt.Errorf("%s CHECK 验证失败: %w", t.Info.TableName, e)
					}
					if valid != 1 {
						return fmt.Errorf("%s 在目标语义下违反 CHECK", t.Info.TableName)
					}
				}
				for i, c := range t.Info.Columns {
					if row[i] == nil {
						continue
					}
					if c.Extra == "auto_increment" {
						if n, ok := row[i].(int64); ok && n < 0 {
							return fmt.Errorf("%s.%s 自增值含负数", t.Info.TableName, c.Name)
						}
					}
					if c.DataType == "json" || c.DataType == "jsonb" {
						var valid int
						if e := db.QueryRowContext(ctx, `SELECT JSON_VALID(?)`, row[i]).Scan(&valid); e != nil {
							return e
						}
						if valid != 1 {
							return fmt.Errorf("%s.%s 包含 MySQL 不接受的 JSON", t.Info.TableName, c.Name)
						}
					}
				}
				for _, idx := range t.Indexes {
					set := sets[idxRef(idx)]
					if set == nil {
						continue
					}
					key, valid, e := pgKey(row, t.Info.Columns, idx.Columns)
					if e != nil {
						return e
					}
					if !valid {
						if idx.IsPrimary {
							return fmt.Errorf("主键 %s 含 NULL", idx.IndexName)
						}
						continue
					}
					if _, duplicate := set[key]; duplicate {
						return &pgObjectError{idxRef(idx), fmt.Errorf("在目标比较规则下违反唯一性（含尾空格差异）")}
					}
					set[key] = struct{}{}
				}
			}
			p.RowsChecked += int64(len(rows))
			p.rowCounts[t.Info.TableName] += int64(len(rows))
			return nil
		})
		if checkStmt != nil {
			checkStmt.Close()
		}
		if e != nil {
			var objectErr *pgObjectError
			if errors.As(e, &objectErr) {
				return e
			}
			return &pgObjectError{tableRef(t.Info.TableName), e}
		}
	}
	// Re-scan foreign-key children after all referenced unique keys are known;
	// never retain entire data rows or compare raw source collation semantics.
	for _, fk := range p.validationFKs {
		t := tableMap[fk.TableName]
		referenced, ok := tableMap[fk.RefTable]
		if !ok {
			return fmt.Errorf("外键 %s 引用目标表未纳入全量预检", fk.ConstraintName)
		}
		var parent pgKeySet
		for _, idx := range referenced.Indexes {
			if idx.IsUnique && strings.Join(idx.Columns, "\x00") == strings.Join(fk.RefColumns, "\x00") {
				parent = sets[idxRef(idx)]
			}
		}
		if parent == nil {
			return fmt.Errorf("外键 %s 缺少已验证的引用键", fk.ConstraintName)
		}
		var inputDB *sql.DB
		if readTarget {
			inputDB = db
		}
		if e := m.eachPGPage(ctx, t, inputDB, func(_ []string, rows [][]any) error {
			for _, row := range rows {
				key, valid, e := pgKey(row, t.Info.Columns, fk.Columns)
				if e != nil {
					return e
				}
				if valid {
					if _, ok := parent[key]; !ok {
						return &pgObjectError{ObjectRef{"foreign_key", fk.TableName, fk.ConstraintName}, fmt.Errorf("外键存在未匹配引用行")}
					}
				}
			}
			return nil
		}); e != nil {
			return e
		}
	}
	return nil
}

func (m *Migrator) pgRowCheckQuery(t source.PGTable, checksSupported bool) (string, error) {
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
	for _, c := range t.Info.Columns {
		typ, e := typemap.PGToMySQLChecked(c)
		if e != nil {
			return "", e
		}
		cast := "?"
		switch {
		case strings.HasPrefix(typ, "DECIMAL"):
			cast = "CAST(? AS " + typ + ")"
		case typ == "INT" || typ == "SMALLINT" || typ == "BIGINT" || strings.HasPrefix(typ, "TINYINT"):
			cast = "CAST(? AS SIGNED)"
		case typ == "DATE" || strings.HasPrefix(typ, "TIME(") || strings.HasPrefix(typ, "DATETIME("):
			cast = "CAST(? AS " + typ + ")"
		case typ == "FLOAT" || typ == "DOUBLE":
			cast = "(? + 0e0)"
		case typ == "JSON":
			cast = "CAST(? AS JSON)"
		case typ == "LONGBLOB":
			cast = "CAST(? AS BINARY)"
		default:
			cast = "CAST(? AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin"
		}
		cols = append(cols, cast+" AS "+sqlsyntax.QuoteIdent(m.objName(c.Name)))
	}
	return "SELECT (" + strings.Join(conditions, " AND ") + ") FROM (SELECT " + strings.Join(cols, ",") + ") AS checked_row", nil
}

// Bound the local InnoDB record after DYNAMIC external storage. Short variable
// values stay inline; long values need a 20-byte external pointer. No truncation
// or row-format guessing is used to bypass this check.
func checkPGRowStorage(t source.PGTable, row []any, pageSize int) error {
	local := 32 + (len(t.Info.Columns)+7)/8
	for i, c := range t.Info.Columns {
		if row[i] == nil {
			continue
		}
		typ, e := typemap.PGToMySQLChecked(c)
		if e != nil {
			return e
		}
		switch {
		case strings.Contains(typ, "CHAR") || typ == "LONGTEXT" || typ == "LONGBLOB" || typ == "JSON":
			n := 0
			if b, ok := row[i].([]byte); ok {
				n = len(b)
			} else {
				n = len(fmt.Sprint(row[i]))
			}
			if n > 40 {
				n = 20
			}
			local += n + 2
		default:
			local += 32
		}
	}
	limit := min(pageSize/2-200, 16000)
	if local > limit {
		return fmt.Errorf("%s 行内数据预计 %d 字节，超过 InnoDB %d 字节安全上限", t.Info.TableName, local, limit)
	}
	return nil
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
			parts[i] = "CONVERT(X'" + hex.EncodeToString([]byte(t.Value)) + "' USING utf8mb4) COLLATE utf8mb4_bin"
		} else {
			parts[i] = t.Text
		}
	}
	return strings.Join(parts, " "), nil
}
