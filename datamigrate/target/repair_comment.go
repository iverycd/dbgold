package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
)

// GenerateRepairComment builds an explicit candidate without executing DDL.
// MySQL MODIFY needs the complete current column definition; reconstructing it
// from information_schema would silently lose defaults, collation and extras.
func GenerateRepairComment(ctx context.Context, w Writer, cm source.CommentInfo) ([]dialect.Statement, error) {
	a, err := repairAccess(w)
	if err != nil {
		return nil, err
	}
	schema, err := ResolveRepairSchema(ctx, w)
	if err != nil {
		return nil, err
	}
	if cm.TableName == "" {
		return nil, fmt.Errorf("注释缺少目标表名")
	}
	if a.family != "mysql" {
		return w.Dialect().CommentStatements(schema, cm), nil
	}
	qualified := w.Dialect().QualifyTable(schema, cm.TableName)
	if cm.ColumnName == "" {
		return []dialect.Statement{{SQL: "ALTER TABLE " + qualified + " COMMENT = " + sqlsyntax.QuoteString(cm.Comment)}}, nil
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }); _ = conn.Close() }()
	// A private read session ensures SHOW CREATE emits quoted identifiers and
	// every attribute (NO_FIELD_OPTIONS would otherwise hide column options).
	// Strings are decoded from its known backslash mode into SQL-standard form.
	if _, err = conn.ExecContext(ctx, "SET SESSION sql_mode='', sql_quote_show_create=1"); err != nil {
		return nil, err
	}
	var shown, ddl string
	if err = conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+qualified).Scan(&shown, &ddl); err != nil {
		return nil, err
	}
	if shown != cm.TableName {
		return nil, fmt.Errorf("SHOW CREATE 返回的目标表身份不一致")
	}
	definition, err := repairMySQLColumnComment(ddl, cm.TableName, cm.ColumnName, cm.Comment)
	if err != nil {
		return nil, err
	}
	return []dialect.Statement{{SQL: "ALTER TABLE " + qualified + " MODIFY COLUMN " + definition}}, nil
}

type repairMySQLToken struct {
	start, end, depth int
	kind              byte
	value             string
}

var repairMySQLVisibility = regexp.MustCompile(`(?i)^/\*![0-9]+\s+(?:INVISIBLE|VISIBLE)\s*\*/$`)

// This delimiter scanner preserves untouched SHOW CREATE bytes. It rejects
// uncertain lexical forms instead of approximating a whole MySQL grammar.
func repairMySQLTokens(s string) ([]repairMySQLToken, error) {
	var out []repairMySQLToken
	depth := 0
	for i := 0; i < len(s); {
		start, c := i, s[i]
		if strings.ContainsRune(" \t\r\n", rune(c)) {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("SHOW CREATE 注释未闭合")
			}
			i += end + 4
			if !repairMySQLVisibility.MatchString(s[start:i]) {
				return nil, fmt.Errorf("SHOW CREATE 含无法安全保留的扩展注释")
			}
			out = append(out, repairMySQLToken{start, i, depth, 'v', s[start:i]})
			continue
		}
		if c == ';' || c == '#' || strings.HasPrefix(s[i:], "--") {
			return nil, fmt.Errorf("SHOW CREATE 含不明确的语句边界")
		}
		if c == '\'' || c == '"' || c == '`' {
			quote := c
			i++
			var value strings.Builder
			closed := false
			for i < len(s) {
				c = s[i]
				i++
				if c == quote {
					if i < len(s) && s[i] == quote {
						value.WriteByte(c)
						i++
						continue
					}
					closed = true
					break
				}
				if c == '\\' && quote != '`' {
					if i >= len(s) {
						return nil, fmt.Errorf("SHOW CREATE 转义未闭合")
					}
					c = s[i]
					i++
					switch c {
					case '0':
						c = 0
					case 'n':
						c = '\n'
					case 'r':
						c = '\r'
					case 't':
						c = '\t'
					case 'b':
						c = '\b'
					case 'Z':
						c = 26
					case '\\', '\'', '"':
					default:
						return nil, fmt.Errorf("SHOW CREATE 含不支持的字符串转义")
					}
				}
				value.WriteByte(c)
			}
			if !closed {
				return nil, fmt.Errorf("SHOW CREATE 引号未闭合")
			}
			kind := byte('s')
			if quote == '`' {
				kind = 'q'
			}
			out = append(out, repairMySQLToken{start, i, depth, kind, value.String()})
			continue
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 128 {
			i++
			for i < len(s) {
				b := s[i]
				if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '$' || b >= 128) {
					break
				}
				i++
			}
			out = append(out, repairMySQLToken{start, i, depth, 'w', s[start:i]})
			continue
		}
		if c == ')' {
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("SHOW CREATE 括号不匹配")
			}
		}
		i++
		out = append(out, repairMySQLToken{start, i, depth, 'p', s[start:i]})
		if c == '(' {
			depth++
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("SHOW CREATE 括号未闭合")
	}
	return out, nil
}

func repairMySQLColumnComment(ddl, table, column, comment string) (string, error) {
	ts, err := repairMySQLTokens(ddl)
	if err != nil {
		return "", err
	}
	if len(ts) < 6 || !strings.EqualFold(ts[0].value, "CREATE") || !strings.EqualFold(ts[1].value, "TABLE") || ts[2].kind != 'q' || ts[2].value != table || ts[3].value != "(" {
		return "", fmt.Errorf("无法确认 SHOW CREATE TABLE 定义")
	}
	start, end, matches := 4, 0, 0
	var cols []repairMySQLToken
	for i := 4; i < len(ts); i++ {
		boundary := ts[i].kind == 'p' && (ts[i].value == "," && ts[i].depth == 1 || ts[i].value == ")" && ts[i].depth == 0)
		if !boundary {
			continue
		}
		if start < i && ts[start].kind == 'q' && ts[start].value == column {
			matches++
			cols = ts[start:i]
			end = ts[i].start
		}
		start = i + 1
		if ts[i].value == ")" {
			break
		}
	}
	if matches != 1 || len(cols) < 2 || cols[1].kind != 'w' {
		return "", fmt.Errorf("SHOW CREATE 未唯一识别列 %s", column)
	}
	commentIndex := -1
	for i, t := range cols {
		if t.kind == 'w' && t.depth == 1 && strings.EqualFold(t.value, "COMMENT") {
			if commentIndex >= 0 || i+1 >= len(cols) || cols[i+1].kind != 's' {
				return "", fmt.Errorf("SHOW CREATE 列 COMMENT 不明确")
			}
			commentIndex = i + 1
		}
	}
	var out strings.Builder
	pos := cols[0].start
	for i, t := range cols {
		if t.kind != 's' {
			continue
		}
		out.WriteString(ddl[pos:t.start])
		v := t.value
		if i == commentIndex {
			v = comment
		}
		out.WriteString(sqlsyntax.QuoteString(v))
		pos = t.end
	}
	out.WriteString(ddl[pos:end])
	definition := strings.TrimSpace(out.String())
	if commentIndex < 0 {
		definition += " COMMENT " + sqlsyntax.QuoteString(comment)
	}
	return definition, nil
}

func inspectRepairComment(ctx context.Context, a repairTarget, obj repair.Object) (repair.Verification, error) {
	if obj.Comment == nil {
		return verification("unknown", "缺少原注释内容，不能核验")
	}
	cm := *obj.Comment
	if obj.Table != "" {
		cm.TableName = obj.Table
	}
	var q string
	args := []any{obj.Schema, cm.TableName}
	column := cm.ColumnName != ""
	switch a.family {
	case "postgres":
		q = `SELECT obj_description(t.oid,'pg_class') FROM pg_class t JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname=$1 AND t.relname=$2 AND t.relkind IN ('r','p')`
		if column {
			q = `SELECT col_description(t.oid,c.attnum) FROM pg_class t JOIN pg_namespace n ON n.oid=t.relnamespace JOIN pg_attribute c ON c.attrelid=t.oid WHERE n.nspname=$1 AND t.relname=$2 AND c.attname=$3 AND NOT c.attisdropped AND c.attnum>0`
		}
	case "mysql":
		q = `SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND TABLE_TYPE='BASE TABLE'`
		if column {
			q = `SELECT COLUMN_COMMENT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?`
		}
	case "oracle", "dameng":
		q = `SELECT COMMENTS FROM ALL_TAB_COMMENTS WHERE OWNER=:1 AND TABLE_NAME=:2`
		if column {
			q = `SELECT COMMENTS FROM ALL_COL_COMMENTS WHERE OWNER=:1 AND TABLE_NAME=:2 AND COLUMN_NAME=:3`
		}
		if a.family == "dameng" {
			q = strings.NewReplacer(":1", "?", ":2", "?", ":3", "?").Replace(q)
		}
	case "sqlserver":
		q = `SELECT CONVERT(nvarchar(max),p.value) FROM sys.tables t JOIN sys.schemas s ON s.schema_id=t.schema_id LEFT JOIN sys.extended_properties p ON p.class=1 AND p.major_id=t.object_id AND p.minor_id=0 AND p.name=N'MS_Description' WHERE s.name=@p1 AND t.name=@p2`
		if column {
			q = `SELECT CONVERT(nvarchar(max),p.value) FROM sys.tables t JOIN sys.schemas s ON s.schema_id=t.schema_id JOIN sys.columns c ON c.object_id=t.object_id LEFT JOIN sys.extended_properties p ON p.class=1 AND p.major_id=t.object_id AND p.minor_id=c.column_id AND p.name=N'MS_Description' WHERE s.name=@p1 AND t.name=@p2 AND c.name=@p3`
		}
	default:
		return verification("unknown", "当前目标缺少注释目录核验能力")
	}
	if column {
		args = append(args, cm.ColumnName)
	}
	var actual sql.NullString
	err := a.db.QueryRowContext(ctx, q, args...).Scan(&actual)
	if err == sql.ErrNoRows {
		return verification("failed", "目标注释所属表或列不存在，或元数据不可见")
	}
	if err != nil {
		return repair.Verification{Status: "unknown", Message: err.Error()}, err
	}
	if actual.String == cm.Comment {
		return verification("succeeded", "目标实际注释与保存的注释内容一致")
	}
	if repairDefinitionEdited(obj) {
		return verification("unknown", "注释 DDL 已编辑，实际值与原注释不同，请核对修订内容")
	}
	return verification("failed", "目标实际注释与保存的注释内容不一致")
}
