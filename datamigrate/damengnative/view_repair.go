package damengnative

import (
	"fmt"
	"strings"
)

// ViewRepairDefinition checks the mutating identity of a single CREATE.
// It returns the column list and AS query for comparison with GET_DDL, which
// can qualify the name and omit OR REPLACE when returning the stored object.
func ViewRepairDefinition(sql, schema, name string) (string, error) {
	if schema == "" || name == "" || len(sql) == 0 || len(sql) > 1<<20 || strings.ContainsRune(sql, 0) {
		return "", fmt.Errorf("普通视图需要明确的目标和不超过 1 MiB 的完整 SQL")
	}
	tokens, err := nativeTokens(sql)
	if err != nil {
		return "", err
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].kind == 'p' && tokens[len(tokens)-1].value == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	for _, t := range tokens {
		if t.kind == 'p' && t.value == ";" {
			return "", fmt.Errorf("视图修复只接受一条 CREATE VIEW")
		}
	}
	i := 0
	take := func(s string) bool {
		if i >= len(tokens) || tokens[i].kind != 'i' || !strings.EqualFold(tokens[i].value, s) {
			return false
		}
		i++
		return true
	}
	ident := func() string {
		if i >= len(tokens) || tokens[i].kind != 'q' && tokens[i].kind != 'i' {
			return ""
		}
		t := tokens[i]
		i++
		if t.kind == 'q' {
			return t.value
		}
		return strings.ToUpper(t.value)
	}
	if !take("CREATE") || take("OR") && !take("REPLACE") {
		return "", fmt.Errorf("请填写完整的 CREATE VIEW 或 CREATE OR REPLACE VIEW")
	}
	take("FORCE")
	if !take("VIEW") {
		return "", fmt.Errorf("仅支持普通视图的 CREATE VIEW")
	}
	obj := ident()
	if i < len(tokens) && tokens[i].kind == 'p' && tokens[i].value == "." {
		i++
		if obj != schema {
			return "", fmt.Errorf("视图必须属于目标模式 %s", schema)
		}
		obj = ident()
	}
	if obj != name {
		return "", fmt.Errorf("视图必须为 %s", Qualified(schema, name))
	}
	tail := tokens[i-1].end
	// DM accepts an optional list of output column names before AS.
	if i < len(tokens) && tokens[i].kind == 'p' && tokens[i].value == "(" {
		i++
		for {
			if ident() == "" || i == len(tokens) {
				return "", fmt.Errorf("视图列清单缺少列名或右括号")
			}
			if tokens[i].kind == 'p' && tokens[i].value == ")" {
				i++
				break
			}
			if tokens[i].kind != 'p' || tokens[i].value != "," {
				return "", fmt.Errorf("视图列清单无效")
			}
			i++
		}
	}
	if !take("AS") || i >= len(tokens) {
		return "", fmt.Errorf("视图缺少 AS 查询体")
	}
	return sql[tail:], nil
}
