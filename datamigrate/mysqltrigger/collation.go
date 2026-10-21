package mysqltrigger

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

func characterKey(schema, table, column string) string {
	return schema + "\x00" + table + "\x00" + column
}

func (c *converter) frozenCharacterMetadata() *source.TriggerCharacterMetadata {
	if len(c.characterProfiles) == 0 && len(c.numericColumns) == 0 {
		return nil
	}
	m := &source.TriggerCharacterMetadata{ServerVersion: c.opt.CharacterMetadata.ServerVersion}
	var keys []string
	for key := range c.characterColumns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m.Columns = append(m.Columns, c.characterColumns[key])
	}
	keys = nil
	for key := range c.characterProfiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m.Profiles = append(m.Profiles, c.characterProfiles[key])
	}
	keys = nil
	for key := range c.numericColumns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m.NumericColumns = append(m.NumericColumns, c.numericColumns[key])
	}
	return m
}

func (c *converter) characterColumn(t *source.TableDDLInfo, col *source.ColumnInfo) (string, error) {
	m := c.opt.CharacterMetadata
	if m == nil {
		return "", fmt.Errorf("字符比较缺少源库字符集、排序规则及权重快照，需要重新读取源元数据")
	}
	var found *source.TriggerColumnCharacter
	for i := range m.Columns {
		v := &m.Columns[i]
		if v.Schema == c.info.Schema && v.Table == t.TableName && v.Name == col.Name {
			if found != nil {
				return "", fmt.Errorf("字符列 %s.%s 的排序规则快照重复", t.TableName, col.Name)
			}
			found = v
		}
	}
	if found == nil {
		return "", fmt.Errorf("字符列 %s.%s 缺少排序规则快照", t.TableName, col.Name)
	}
	if found.Charset != "utf8mb4" || found.Collation != "utf8mb4_general_ci" {
		return "", fmt.Errorf("字符列 %s.%s 的排序规则 %s 需要人工适配", t.TableName, col.Name, found.Collation)
	}
	var profile *source.TriggerCollationProfile
	for i := range m.Profiles {
		v := &m.Profiles[i]
		if v.Charset == found.Charset && v.Collation == found.Collation {
			if profile != nil {
				return "", fmt.Errorf("字符排序规则权重快照重复")
			}
			profile = v
		}
	}
	if profile == nil {
		return "", fmt.Errorf("排序规则 %s 缺少源权重快照", found.Collation)
	}
	check := &source.TriggerCharacterMetadata{ServerVersion: m.ServerVersion, Columns: []source.TriggerColumnCharacter{*found}, Profiles: []source.TriggerCollationProfile{*profile}}
	if err := source.ValidateTriggerCharacterMetadata(check); err != nil {
		return "", fmt.Errorf("源排序规则权重快照不可用: %w", err)
	}
	c.characterColumns[characterKey(found.Schema, found.Table, found.Name)] = *found
	c.characterProfiles[profile.Collation] = *profile
	return found.Collation, nil
}

// An empty collation is a weak operand (a literal or numeric CONCAT argument).
// Only a known source character column may select the effective collation.
func (c *converter) expressionCollation(n ast.ExprNode, t *source.TableDDLInfo) (string, error) {
	switch e := n.(type) {
	case ast.ValueExpr:
		return "", nil
	case *ast.ParenthesesExpr:
		return c.expressionCollation(e.Expr, t)
	case *ast.UnaryOperationExpr:
		return c.expressionCollation(e.V, t)
	case *ast.ColumnNameExpr:
		owner := t
		if strings.EqualFold(e.Name.Table.O, "OLD") || strings.EqualFold(e.Name.Table.O, "NEW") {
			owner = c.tables[c.info.TableName]
		}
		col, err := c.column(owner, e.Name.Name.O)
		if err != nil {
			return "", err
		}
		if kindOf(col.DataType) != kindText {
			return "", nil
		}
		return c.characterColumn(owner, col)
	case *ast.FuncCallExpr:
		fn := strings.ToLower(e.FnName.O)
		if fn != "concat" && fn != "coalesce" && fn != "ifnull" {
			return "", fmt.Errorf("表达式的字符排序规则无法确定")
		}
		var chosen string
		for _, arg := range e.Args {
			collation, err := c.expressionCollation(arg, t)
			if err != nil {
				return "", err
			}
			if collation != "" {
				if chosen != "" && chosen != collation {
					return "", fmt.Errorf("混合字符排序规则需要人工适配")
				}
				chosen = collation
			}
		}
		return chosen, nil
	}
	return "", fmt.Errorf("表达式的字符排序规则无法确定")
}

func (c *converter) textComparison(n *ast.BinaryOperationExpr, a, b expression, t *source.TableDDLInfo) (expression, error) {
	if n.Op != opcode.EQ && n.Op != opcode.NE {
		return expression{}, fmt.Errorf("字符比较仅支持等值和不等值，其他排序语义需要人工适配")
	}
	left, err := c.expressionCollation(n.L, t)
	if err != nil {
		return expression{}, err
	}
	right, err := c.expressionCollation(n.R, t)
	if err != nil {
		return expression{}, err
	}
	if left != "" && right != "" && left != right {
		return expression{}, fmt.Errorf("比较双方字符排序规则不同，需要人工适配")
	}
	if left == "" {
		left = right
	}
	if left == "" {
		return expression{}, fmt.Errorf("字符比较缺少可确定的源列排序规则")
	}
	p, ok := c.characterProfiles[left]
	if !ok {
		return expression{}, fmt.Errorf("缺少源排序规则权重快照")
	}
	op := "="
	if n.Op == opcode.NE {
		op = "<>"
	}
	return expression{"(" + collationKeySQL(a.sql, p) + " " + op + " " + collationKeySQL(b.sql, p) + ")", kindBool}, nil
}

func hexText(s string) string {
	return "pg_catalog.convert_from(pg_catalog.decode('" + hex.EncodeToString([]byte(s)) + "', 'hex'), 'UTF8')"
}

// utf8mb4_general_ci compares one weight per scalar and ignores only trailing
// U+0020. Captured BMP mappings are sparse and applied once, not recursively.
// Every supplementary scalar has the frozen FFFD weight. bytea equality keeps
// the target database's own locale and collation out of the comparison.
func collationKeySQL(value string, p source.TriggerCollationProfile) string {
	trimmed := "pg_catalog.rtrim(CAST(" + value + " AS text), ' ')"
	pattern := hexText("[" + string(rune(0x10000)) + "-" + string(rune(0x10ffff)) + "]")
	unicode := "pg_catalog.regexp_replace(" + trimmed + " COLLATE pg_catalog.\"C\", " + pattern + ", " + hexText(string(rune(p.SupplementaryWeight))) + ", 'g')"
	weighted := "pg_catalog.translate(" + unicode + ", " + hexText(p.From) + ", " + hexText(p.To) + ")"
	return "pg_catalog.convert_to(" + weighted + ", 'UTF8')"
}
