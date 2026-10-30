package source

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

type mysqlKeyDeclaration struct {
	method    string
	algorithm int
	columns   []string
}

type mysqlKeyIndexColumn struct {
	name     string
	prefix   bool
	nullable bool
}

// MySQLPartitionKeyVersion is shared by source and target capability checks.
// Vendor-compatible version strings alone do not prove an identical hash.
func MySQLPartitionKeyVersion(version, comment string) (string, bool) {
	if !supportedTriggerMySQLServer(version, comment) {
		return "", false
	}
	v := regexp.MustCompile(`^(?:5\.7|8\.0)\.[0-9]+`).FindString(version)
	return v, v != ""
}

// PartitionHasChecks provides the exact KEY domain preflight with an explicit
// native constraint-presence check. Until CHECK expressions are frozen and
// translated as part of the plan, they cannot be silently omitted. Both
// enforced and unenforced constraints are reported conservatively.
func (r *MySQLReader) PartitionHasChecks(ctx context.Context, table string) (bool, error) {
	var count int64
	if err := r.queryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_TYPE='CHECK'`, r.dbName, table).Scan(&count); err != nil {
		return false, fmt.Errorf("读取 MySQL 分区表 CHECK 约束目录: %w", err)
	}
	if count < 0 {
		return false, fmt.Errorf("MySQL CHECK 约束数量无效")
	}
	return count > 0, nil
}

// KEY's engine-provided hash is not interchangeable with HASH. SHOW CREATE
// omits its default algorithm 2, and emits algorithm 1 inside a versioned
// executable comment. Empty KEY() uses the native primary/first eligible unique
// index. Freeze those facts before exposing an executable partition plan.
func (r *MySQLReader) freezeMySQLPartitionKeys(ctx context.Context, table, ddl string, columns map[string]PartitionKey, columnOrder []string, root *PartitionSpec) error {
	var levels []*PartitionSpec
	if root.Method == "KEY" || root.Method == "LINEAR KEY" {
		levels = append(levels, root)
	}
	for i := range root.Partitions {
		sub := root.Partitions[i].Subpartition
		if sub != nil && (sub.Method == "KEY" || sub.Method == "LINEAR KEY") {
			levels = append(levels, sub)
		}
	}
	if len(levels) == 0 {
		return nil
	}
	var version, comment, mode, engine string
	if err := r.queryRowContext(ctx, `SELECT VERSION(),@@version_comment,@@SESSION.sql_mode`).Scan(&version, &comment, &mode); err != nil {
		return fmt.Errorf("读取 MySQL KEY 算法版本失败: %w", err)
	}
	nativeVersion, supported := MySQLPartitionKeyVersion(version, comment)
	if !supported {
		return fmt.Errorf("KEY 原生算法仅适配官方 MySQL 5.7/8.0，当前 %s %s", version, comment)
	}
	if err := r.queryRowContext(ctx, `SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, r.dbName, table).Scan(&engine); err != nil {
		return fmt.Errorf("读取 KEY 存储引擎失败: %w", err)
	}
	if !strings.EqualFold(engine, "InnoDB") {
		return fmt.Errorf("MySQL KEY 存储引擎 %s 的原生散列/隐藏键不能替换为 InnoDB 算法", engine)
	}
	declarations, indexOrder, err := mysqlPartitionKeyDeclarations(ddl, strings.Contains(strings.ToUpper(mode), "NO_BACKSLASH_ESCAPES"))
	if err != nil {
		return err
	}
	indexes, err := r.mysqlPartitionKeyIndexes(ctx, table)
	if err != nil {
		return err
	}
	for _, spec := range levels {
		level := "PARTITION"
		if spec != root {
			level = "SUBPARTITION"
		}
		declaration, ok := declarations[level]
		if !ok || declaration.method != spec.Method {
			return fmt.Errorf("MySQL %s KEY 目录与原始 DDL 方法不一致或缺失", level)
		}
		names := declaration.columns
		if len(names) == 0 {
			if len(spec.Keys) != 0 {
				return fmt.Errorf("KEY() 隐式声明与目录显式列不一致")
			}
			selected := indexes["PRIMARY"]
			if len(selected) == 0 {
				for _, index := range indexOrder {
					candidate := indexes[index]
					valid := len(candidate) > 0
					for _, column := range candidate {
						valid = valid && !column.nullable && !column.prefix
					}
					if valid {
						selected = candidate
						break
					}
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("KEY() 没有可冻结的显式主键或完整 NOT NULL 唯一键，不能猜测隐藏键")
			}
			for _, column := range selected {
				if column.prefix || column.nullable {
					return fmt.Errorf("KEY() 隐式键含前缀/可空索引列，其实际参与散列的列尚不能完整保留")
				}
				names = append(names, column.name)
			}
		} else if len(names) != len(spec.Keys) {
			return fmt.Errorf("KEY 显式列数量在原生 DDL 与目录中不一致")
		}
		var resolved []PartitionKey
		seen := map[string]bool{}
		for i, name := range names {
			key, ok := mysqlNativeKeyColumn(columns, name)
			if !ok || seen[key.Column] {
				return fmt.Errorf("KEY 列 %s 不存在、重名或不能按原始列身份解析", name)
			}
			seen[key.Column] = true
			if len(declaration.columns) > 0 {
				catalogName := spec.Keys[i].Column
				if catalogName == "" {
					tokens, e := sqlsyntax.Lex(spec.Keys[i].Expression)
					if e != nil || len(tokens) != 1 || tokens[0].Kind != 'i' && tokens[0].Kind != 'q' {
						return fmt.Errorf("KEY 原生列目录包含表达式，不能替换为普通列")
					}
					catalogName = mysqlKeyIdentifier(tokens[0])
				}
				catalogKey, found := mysqlNativeKeyColumn(columns, catalogName)
				if !found || catalogKey.Column != key.Column {
					return fmt.Errorf("KEY 列顺序在原生 DDL 与目录中不一致")
				}
			}
			for _, index := range indexes {
				for _, indexed := range index {
					if indexed.name == key.Column && indexed.prefix {
						return fmt.Errorf("KEY 列 %s 参与前缀唯一索引，其原生散列列集合尚不能等价恢复", key.Column)
					}
				}
			}
			resolved = append(resolved, key)
		}
		// Native set_up_field_array hashes in table field order, not declaration
		// or primary-index order. Freeze that order for strict append checks.
		byName := map[string]PartitionKey{}
		for _, key := range resolved {
			byName[key.Column] = key
		}
		resolved = nil
		for _, name := range columnOrder {
			if key, exists := byName[name]; exists {
				resolved = append(resolved, key)
				delete(byName, name)
			}
		}
		if len(byName) != 0 {
			return fmt.Errorf("KEY 物理列顺序目录不完整")
		}
		spec.Keys = resolved
		spec.Algorithm = "mysql-key-innodb-" + strconv.Itoa(declaration.algorithm) + ":" + nativeVersion
	}
	return nil
}

func mysqlNativeKeyColumn(columns map[string]PartitionKey, name string) (PartitionKey, bool) {
	if key, ok := columns[name]; ok {
		return key, true
	}
	var result PartitionKey
	count := 0
	for original, key := range columns {
		if strings.EqualFold(original, name) {
			result, count = key, count+1
		}
	}
	return result, count == 1
}

func (r *MySQLReader) mysqlPartitionKeyIndexes(ctx context.Context, table string) (map[string][]mysqlKeyIndexColumn, error) {
	rows, err := r.queryContext(ctx, `SELECT INDEX_NAME,SEQ_IN_INDEX,COLUMN_NAME,SUB_PART,NULLABLE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND NON_UNIQUE=0 ORDER BY INDEX_NAME,SEQ_IN_INDEX`, r.dbName, table)
	if err != nil {
		return nil, fmt.Errorf("读取 KEY 原生主键/唯一键失败: %w", err)
	}
	defer rows.Close()
	out := map[string][]mysqlKeyIndexColumn{}
	for rows.Next() {
		var index, nullable string
		var column sql.NullString
		var prefix sql.NullInt64
		var position int
		if err = rows.Scan(&index, &position, &column, &prefix, &nullable); err != nil {
			return nil, err
		}
		if position != len(out[index])+1 || !column.Valid {
			return nil, fmt.Errorf("KEY 唯一键 %s 包含表达式或列顺序不完整", index)
		}
		out[index] = append(out[index], mysqlKeyIndexColumn{name: column.String, prefix: prefix.Valid, nullable: nullable != ""})
	}
	return out, rows.Err()
}

func mysqlKeyIdentifier(token sqlsyntax.Token) string {
	if token.Kind == 'q' {
		return token.Value
	}
	return token.Text
}

func mysqlPartitionKeyDeclarations(ddl string, noBackslash bool) (map[string]mysqlKeyDeclaration, []string, error) {
	flat, err := mysqlExpandPartitionComments(ddl, noBackslash)
	if err != nil {
		return nil, nil, err
	}
	tokens, err := sqlsyntax.Lex(flat)
	if err != nil {
		return nil, nil, fmt.Errorf("KEY 原生 DDL 无法完整分词: %w", err)
	}
	out := map[string]mysqlKeyDeclaration{}
	var indexes []string
	depth := 0
	for i := 0; i < len(tokens); i++ {
		if tokens[i].Is("(") {
			depth++
		} else if tokens[i].Is(")") {
			depth--
		}
		if depth == 1 && tokens[i].Is("UNIQUE") && i+2 < len(tokens) && (tokens[i+1].Is("KEY") || tokens[i+1].Is("INDEX")) {
			indexes = append(indexes, mysqlKeyIdentifier(tokens[i+2]))
		}
		if depth != 0 || i+2 >= len(tokens) || !(tokens[i].Is("PARTITION") || tokens[i].Is("SUBPARTITION")) || !tokens[i+1].Is("BY") {
			continue
		}
		level, j := strings.ToUpper(tokens[i].Text), i+2
		decl := mysqlKeyDeclaration{method: "KEY", algorithm: 2}
		if tokens[j].Is("LINEAR") {
			decl.method, j = "LINEAR KEY", j+1
		}
		if j >= len(tokens) || !tokens[j].Is("KEY") {
			continue
		}
		j++
		if j < len(tokens) && tokens[j].Is("ALGORITHM") {
			if j+2 >= len(tokens) || !tokens[j+1].Is("=") || tokens[j+2].Text != "1" && tokens[j+2].Text != "2" {
				return nil, nil, fmt.Errorf("KEY ALGORITHM 不是已验证的 1/2")
			}
			decl.algorithm, _ = strconv.Atoi(tokens[j+2].Text)
			j += 3
		}
		if j >= len(tokens) || !tokens[j].Is("(") {
			return nil, nil, fmt.Errorf("KEY 列表缺少括号")
		}
		j++
		for j < len(tokens) && !tokens[j].Is(")") {
			if tokens[j].Kind != 'i' && tokens[j].Kind != 'q' {
				return nil, nil, fmt.Errorf("KEY 只接受原生直接列名称")
			}
			decl.columns = append(decl.columns, mysqlKeyIdentifier(tokens[j]))
			j++
			if j < len(tokens) && tokens[j].Is(",") {
				j++
				if j >= len(tokens) || tokens[j].Is(")") {
					return nil, nil, fmt.Errorf("KEY 列表含空列")
				}
			} else if j >= len(tokens) || !tokens[j].Is(")") {
				return nil, nil, fmt.Errorf("KEY 列表包含表达式或未知选项")
			}
		}
		if j >= len(tokens) || out[level].method != "" {
			return nil, nil, fmt.Errorf("KEY 原生声明不完整或重复")
		}
		out[level] = decl
	}
	return out, indexes, nil
}

// Unwrap only executable comments outside quoted literals/identifiers. SHOW
// CREATE emits KEY algorithm 1 in its own /*!50611 ... */ fragment. Ordinary
// comments and comment-looking text in defaults never become SQL tokens.
func mysqlExpandPartitionComments(sqlText string, noBackslash bool) (string, error) {
	var out strings.Builder
	for i := 0; i < len(sqlText); {
		if sqlText[i] == '\'' || sqlText[i] == '"' || sqlText[i] == '`' {
			start, quote := i, sqlText[i]
			i++
			closed := false
			for i < len(sqlText) {
				if sqlText[i] == '\\' && quote != '`' && !noBackslash {
					i += 2
					continue
				}
				if sqlText[i] == quote {
					i++
					if i < len(sqlText) && sqlText[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed || i > len(sqlText) {
				return "", fmt.Errorf("KEY 原生 DDL 引号不完整")
			}
			out.WriteString(sqlText[start:i])
			continue
		}
		if strings.HasPrefix(sqlText[i:], "/*") {
			end := strings.Index(sqlText[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("KEY 原生 DDL 注释不完整")
			}
			end += i + 2
			if strings.HasPrefix(sqlText[i:], "/*!") {
				start := i + 3
				for start < end && sqlText[start] >= '0' && sqlText[start] <= '9' {
					start++
				}
				out.WriteByte(' ')
				out.WriteString(sqlText[start:end])
				out.WriteByte(' ')
			} else {
				out.WriteByte(' ')
			}
			i = end + 2
			continue
		}
		out.WriteByte(sqlText[i])
		i++
	}
	return out.String(), nil
}
