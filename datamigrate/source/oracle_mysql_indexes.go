package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Keep the LONG expression out of joins and SQL functions. Oracle's dictionary
// restrictions on LONG do not apply to either of these ordinary metadata reads.
const oracleMySQLIndexesSQL = `SELECT i.TABLE_NAME,i.INDEX_NAME,ic.COLUMN_NAME,i.UNIQUENESS,
 ic.COLUMN_POSITION,ic.DESCEND,i.INDEX_TYPE,i.STATUS
 FROM ALL_INDEXES i LEFT JOIN ALL_IND_COLUMNS ic
 ON ic.INDEX_OWNER=i.OWNER AND ic.INDEX_NAME=i.INDEX_NAME
 AND ic.TABLE_OWNER=i.TABLE_OWNER AND ic.TABLE_NAME=i.TABLE_NAME
 WHERE i.OWNER=:1 AND i.TABLE_OWNER=:2 AND NOT EXISTS (
 SELECT 1 FROM ALL_CONSTRAINTS c WHERE c.OWNER=i.TABLE_OWNER
 AND c.TABLE_NAME=i.TABLE_NAME AND c.INDEX_OWNER=i.OWNER
 AND c.INDEX_NAME=i.INDEX_NAME AND c.CONSTRAINT_TYPE='P')
 ORDER BY i.TABLE_NAME,i.INDEX_NAME,ic.COLUMN_POSITION`

const oracleMySQLIndexColumnsSQL = `SELECT TABLE_NAME,COLUMN_NAME,HIDDEN_COLUMN,VIRTUAL_COLUMN,NULLABLE
 FROM ALL_TAB_COLS WHERE OWNER=:1 ORDER BY TABLE_NAME,INTERNAL_COLUMN_ID`

const oracleMySQLIndexExpressionsSQL = `SELECT TABLE_NAME,INDEX_NAME,COLUMN_POSITION,COLUMN_EXPRESSION
 FROM ALL_IND_EXPRESSIONS WHERE INDEX_OWNER=:1 AND TABLE_OWNER=:2
 ORDER BY TABLE_NAME,INDEX_NAME,COLUMN_POSITION`

type oracleMySQLSourceIndexID struct{ table, name string }
type oracleMySQLSourceColumnID struct{ table, name string }
type oracleMySQLSourceExpressionID struct {
	oracleMySQLSourceIndexID
	position int64
}

type oracleMySQLSourceIndexKey struct {
	column, direction sql.NullString
	position          sql.NullInt64
}

type oracleMySQLSourceIndex struct {
	index      IndexInfo
	keys       []oracleMySQLSourceIndexKey
	uniqueness sql.NullString
	kind       sql.NullString
	status     sql.NullString
	reasons    []string
}

type oracleMySQLSourceColumn struct {
	physical      bool
	hidden        bool
	flagsKnown    bool
	nullableKnown bool
	notNull       bool
	duplicate     bool
}

type oracleMySQLSourceExpression struct {
	text      sql.NullString
	duplicate bool
}

// GetIndexesForMySQL is an optional, target-specific capability. The historical
// GetIndexes result and the Oracle -> PostgreSQL / Dameng paths stay independent.
// Every returned key is a real physical source column, or its whole index is
// explicitly unsupported; source expressions are never executable target SQL.
func (r *OracleReader) GetIndexesForMySQL(ctx context.Context) ([]IndexInfo, error) {
	indexes, err := r.oracleMySQLSourceIndexes(ctx)
	if err != nil || len(indexes) == 0 {
		return nil, err
	}
	columns, err := r.oracleMySQLSourceColumns(ctx)
	if err != nil {
		return nil, err
	}
	needsExpressions := false
	for _, item := range indexes {
		for _, key := range item.keys {
			column, exists := columns[oracleMySQLSourceColumnID{item.index.TableName, key.column.String}]
			if exists && column.flagsKnown && !column.duplicate && column.hidden &&
				key.column.Valid && key.column.String != "" && oracleMySQLSourceIndexKind(item.kind) {
				needsExpressions = true
			}
		}
	}
	var expressions map[oracleMySQLSourceExpressionID]oracleMySQLSourceExpression
	var expressionErr error
	if needsExpressions {
		expressions, expressionErr = r.oracleMySQLSourceExpressions(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	out := make([]IndexInfo, 0, len(indexes))
	for _, item := range indexes {
		idx := item.index
		reasons := append([]string(nil), item.reasons...)
		resolved := make([]bool, len(item.keys))
		for i, key := range item.keys {
			column, exists := columns[oracleMySQLSourceColumnID{idx.TableName, key.column.String}]
			if !key.column.Valid || key.column.String == "" {
				reasons = append(reasons, fmt.Sprintf("第 %d 个键缺少列名", i+1))
				continue
			}
			if !exists || !column.flagsKnown || column.duplicate {
				reasons = append(reasons, fmt.Sprintf("键列 %s 的源列元数据缺失或不完整", key.column.String))
				continue
			}
			if column.physical {
				resolved[i] = true
				continue
			}
			if !column.hidden {
				reasons = append(reasons, fmt.Sprintf("键列 %s 是虚拟列，暂不支持自动迁移", key.column.String))
				continue
			}
			if !oracleMySQLSourceIndexKind(item.kind) {
				continue
			}
			if expressionErr != nil {
				reasons = append(reasons, fmt.Sprintf("键列 %s 的表达式元数据读取失败: %v", key.column.String, expressionErr))
				continue
			}
			expression, exists := expressions[oracleMySQLSourceExpressionID{
				oracleMySQLSourceIndexID{idx.TableName, idx.IndexName}, key.position.Int64,
			}]
			if !exists || expression.duplicate || !expression.text.Valid || strings.TrimSpace(expression.text.String) == "" {
				reasons = append(reasons, fmt.Sprintf("隐藏或虚拟键列 %s 缺少完整的原始表达式元数据", key.column.String))
				continue
			}
			name, descending, parseErr := oracleMySQLIndexExpressionColumn(expression.text.String)
			if parseErr == nil && descending && (!key.direction.Valid || key.direction.String != "DESC") {
				parseErr = fmt.Errorf("SYS_OP_DESCEND 与目录中的索引方向不一致")
			}
			actual, found := columns[oracleMySQLSourceColumnID{idx.TableName, name}]
			if parseErr == nil && (!found || !actual.flagsKnown || actual.duplicate || !actual.physical) {
				parseErr = fmt.Errorf("引用列 %s 不是可见的普通物理列", name)
			}
			if parseErr != nil {
				reasons = append(reasons, fmt.Sprintf("键列 %s 的原始表达式 %q 暂不支持自动迁移: %v", key.column.String, expression.text.String, parseErr))
				continue
			}
			idx.Columns[i] = name
			resolved[i] = true
		}
		if idx.IsUnique && oracleMySQLHasDescending(idx.Descending) {
			for i, name := range idx.Columns {
				column := columns[oracleMySQLSourceColumnID{idx.TableName, name}]
				if !resolved[i] || !column.nullableKnown || !column.notNull {
					reasons = append(reasons, "Oracle 降序唯一索引的 NULL 唯一性语义不同，只有所有键列均明确为 NOT NULL 时才能自动迁移")
					break
				}
			}
		}
		if len(reasons) > 0 {
			idx.Unsupported = fmt.Sprintf("Oracle 索引 %s: %s", idx.IndexName, strings.Join(reasons, "；"))
		}
		out = append(out, idx)
	}
	return out, nil
}

func oracleMySQLSourceIndexKind(kind sql.NullString) bool {
	return kind.Valid && (kind.String == "NORMAL" || kind.String == "FUNCTION-BASED NORMAL")
}

func oracleMySQLHasDescending(directions []bool) bool {
	for _, descending := range directions {
		if descending {
			return true
		}
	}
	return false
}

func (r *OracleReader) oracleMySQLSourceIndexes(ctx context.Context) ([]oracleMySQLSourceIndex, error) {
	rows, err := r.db.QueryContext(ctx, oracleMySQLIndexesSQL, r.owner, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []oracleMySQLSourceIndex
	positions := map[oracleMySQLSourceIndexID]int{}
	for rows.Next() {
		var table, name string
		var key oracleMySQLSourceIndexKey
		var uniqueness, kind, status sql.NullString
		if err := rows.Scan(&table, &name, &key.column, &uniqueness, &key.position, &key.direction, &kind, &status); err != nil {
			return nil, err
		}
		if table == "" || name == "" {
			return nil, fmt.Errorf("Oracle 索引目录缺少表名或索引名")
		}
		id := oracleMySQLSourceIndexID{table, name}
		position, exists := positions[id]
		if !exists {
			position = len(out)
			positions[id] = position
			out = append(out, oracleMySQLSourceIndex{
				index:      IndexInfo{TableName: table, IndexName: name, IsUnique: uniqueness.Valid && uniqueness.String == "UNIQUE", Method: kind.String},
				uniqueness: uniqueness, kind: kind, status: status,
			})
		}
		item := &out[position]
		if uniqueness != item.uniqueness || kind != item.kind || status != item.status {
			item.reasons = append(item.reasons, "同一索引的类型、状态或唯一性元数据不一致")
		}
		if !uniqueness.Valid || uniqueness.String != "UNIQUE" && uniqueness.String != "NONUNIQUE" {
			item.reasons = append(item.reasons, "唯一性元数据缺失或无效")
		}
		if !oracleMySQLSourceIndexKind(kind) {
			item.reasons = append(item.reasons, fmt.Sprintf("索引类型 %q 暂不支持自动迁移", kind.String))
		}
		if !status.Valid || status.String != "VALID" && status.String != "N/A" {
			item.reasons = append(item.reasons, fmt.Sprintf("索引状态 %q 暂不支持自动迁移", status.String))
		}
		if !key.position.Valid || key.position.Int64 != int64(len(item.keys)+1) {
			item.reasons = append(item.reasons, "键列顺序元数据缺失或不完整")
		}
		if !key.direction.Valid || key.direction.String != "ASC" && key.direction.String != "DESC" {
			item.reasons = append(item.reasons, "键列方向元数据缺失或无效")
		}
		item.keys = append(item.keys, key)
		item.index.Columns = append(item.index.Columns, key.column.String)
		item.index.Descending = append(item.index.Descending, key.direction.Valid && key.direction.String == "DESC")
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *OracleReader) oracleMySQLSourceColumns(ctx context.Context) (map[oracleMySQLSourceColumnID]oracleMySQLSourceColumn, error) {
	rows, err := r.db.QueryContext(ctx, oracleMySQLIndexColumnsSQL, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[oracleMySQLSourceColumnID]oracleMySQLSourceColumn{}
	for rows.Next() {
		var table, name string
		var hidden, virtual, nullable sql.NullString
		if err := rows.Scan(&table, &name, &hidden, &virtual, &nullable); err != nil {
			return nil, err
		}
		if table == "" || name == "" {
			return nil, fmt.Errorf("Oracle 列目录缺少表名或列名")
		}
		id := oracleMySQLSourceColumnID{table, name}
		_, duplicate := out[id]
		out[id] = oracleMySQLSourceColumn{
			physical: hidden.Valid && hidden.String == "NO" && virtual.Valid && virtual.String == "NO",
			hidden:   hidden.Valid && hidden.String == "YES",
			flagsKnown: hidden.Valid && (hidden.String == "YES" || hidden.String == "NO") &&
				virtual.Valid && (virtual.String == "YES" || virtual.String == "NO"),
			nullableKnown: nullable.Valid && (nullable.String == "Y" || nullable.String == "N"),
			notNull:       nullable.Valid && nullable.String == "N",
			duplicate:     duplicate,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *OracleReader) oracleMySQLSourceExpressions(ctx context.Context) (map[oracleMySQLSourceExpressionID]oracleMySQLSourceExpression, error) {
	rows, err := r.db.QueryContext(ctx, oracleMySQLIndexExpressionsSQL, r.owner, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[oracleMySQLSourceExpressionID]oracleMySQLSourceExpression{}
	for rows.Next() {
		var table, name string
		var position int64
		var expression sql.NullString
		if err := rows.Scan(&table, &name, &position, &expression); err != nil {
			return nil, err
		}
		id := oracleMySQLSourceExpressionID{oracleMySQLSourceIndexID{table, name}, position}
		_, duplicate := out[id]
		out[id] = oracleMySQLSourceExpression{text: expression, duplicate: duplicate}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// This is intentionally a complete, tiny Oracle grammar, not an expression
// rewriter: a column identifier, optionally wrapped once by SYS_OP_DESCEND.
func oracleMySQLIndexExpressionColumn(raw string) (string, bool, error) {
	input := strings.TrimSpace(raw)
	name, quoted, rest, err := oracleMySQLIndexIdentifier(input)
	if err != nil {
		return "", false, err
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		if !quoted {
			name = strings.ToUpper(name)
		}
		return name, false, nil
	}
	if quoted || !strings.EqualFold(name, "SYS_OP_DESCEND") || !strings.HasPrefix(rest, "(") {
		return "", false, fmt.Errorf("仅支持普通列或 SYS_OP_DESCEND(普通列)")
	}
	name, quoted, rest, err = oracleMySQLIndexIdentifier(strings.TrimSpace(rest[1:]))
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(rest) != ")" {
		return "", false, fmt.Errorf("SYS_OP_DESCEND 只允许一个普通列参数")
	}
	if !quoted {
		name = strings.ToUpper(name)
	}
	return name, true, nil
}

func oracleMySQLIndexIdentifier(input string) (string, bool, string, error) {
	if input == "" {
		return "", false, "", fmt.Errorf("缺少列标识符")
	}
	if input[0] == '"' {
		var name strings.Builder
		for i := 1; i < len(input); i++ {
			if input[i] != '"' {
				name.WriteByte(input[i])
				continue
			}
			if i+1 < len(input) && input[i+1] == '"' {
				name.WriteByte('"')
				i++
				continue
			}
			if name.Len() == 0 {
				return "", false, "", fmt.Errorf("列标识符为空")
			}
			return name.String(), true, input[i+1:], nil
		}
		return "", false, "", fmt.Errorf("列标识符引号未闭合")
	}
	first, size := utf8.DecodeRuneInString(input)
	if !unicode.IsLetter(first) {
		return "", false, "", fmt.Errorf("无效的普通列标识符")
	}
	i := size
	for i < len(input) {
		character, size := utf8.DecodeRuneInString(input[i:])
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '_' && character != '$' && character != '#' {
			break
		}
		i += size
	}
	return input[:i], false, input[i:], nil
}
