package handler

import (
	"fmt"
	"strings"
	"unicode"
)

type queryAnalysis struct {
	StatementType string
	RiskLevel     string
	ReturnsRows   bool
}

var transactionKeywords = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "ROLLBACK": true, "SAVEPOINT": true, "RELEASE": true,
}

var readKeywords = map[string]bool{
	"SELECT": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXPLAIN": true,
}

var writeKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "REPLACE": true,
}

func analyzeQuerySQL(sqlText string) (queryAnalysis, error) {
	trimmed := strings.TrimSpace(sqlText)
	if trimmed == "" {
		return queryAnalysis{}, fmt.Errorf("SQL 不能为空")
	}
	if len(trimmed) > 1<<20 {
		return queryAnalysis{}, fmt.Errorf("SQL 长度不能超过 1 MiB")
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "DELIMITER") {
		return queryAnalysis{}, fmt.Errorf("首版不支持 DELIMITER 脚本")
	}

	tokens, statementCount, err := scanQueryTokens(trimmed)
	if err != nil {
		return queryAnalysis{}, err
	}
	if statementCount != 1 {
		return queryAnalysis{}, fmt.Errorf("每次只能执行一个 SQL 语句")
	}
	if len(tokens) == 0 {
		return queryAnalysis{}, fmt.Errorf("SQL 不能为空")
	}

	keyword := tokens[0]
	if keyword == "WITH" {
		keyword = "SELECT"
		// A data-modifying CTE is a write even when its final statement is SELECT.
		for _, token := range tokens[1:] {
			if writeKeywords[token] {
				keyword = token
				break
			}
		}
	}
	if transactionKeywords[keyword] {
		return queryAnalysis{}, fmt.Errorf("查询中心不支持跨请求事务控制语句")
	}

	a := queryAnalysis{StatementType: strings.ToLower(keyword), RiskLevel: "dangerous"}
	if readKeywords[keyword] {
		a.RiskLevel = "readonly"
		a.ReturnsRows = true
		// SELECT ... INTO OUTFILE/DUMPFILE has side effects despite its leading keyword.
		for i, token := range tokens {
			if token == "INTO" && i+1 < len(tokens) && (tokens[i+1] == "OUTFILE" || tokens[i+1] == "DUMPFILE") {
				a.RiskLevel = "dangerous"
			}
		}
	} else if writeKeywords[keyword] {
		a.RiskLevel = "write"
		for _, token := range tokens {
			if token == "RETURNING" {
				a.ReturnsRows = true
				break
			}
		}
	}
	return a, nil
}

// scanQueryTokens returns top-level word tokens and the number of top-level
// statements. Quoted text, comments and PostgreSQL dollar-quoted bodies are ignored.
func scanQueryTokens(sqlText string) ([]string, int, error) {
	var tokens []string
	statements := 0
	hasContent := false
	depth := 0
	for i := 0; i < len(sqlText); {
		ch := sqlText[i]
		if unicode.IsSpace(rune(ch)) {
			i++
			continue
		}
		if ch == '-' && i+1 < len(sqlText) && sqlText[i+1] == '-' {
			i += 2
			for i < len(sqlText) && sqlText[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '#' {
			i++
			for i < len(sqlText) && sqlText[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < len(sqlText) && sqlText[i+1] == '*' {
			end := strings.Index(sqlText[i+2:], "*/")
			if end < 0 {
				return nil, 0, fmt.Errorf("SQL 块注释未闭合")
			}
			i += end + 4
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			hasContent = true
			quote := ch
			i++
			closed := false
			for i < len(sqlText) {
				if sqlText[i] == '\\' && quote == '\'' && i+1 < len(sqlText) {
					i += 2
					continue
				}
				if sqlText[i] == quote {
					if i+1 < len(sqlText) && sqlText[i+1] == quote {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, 0, fmt.Errorf("SQL 引号未闭合")
			}
			continue
		}
		if ch == '$' {
			j := i + 1
			for j < len(sqlText) && (unicode.IsLetter(rune(sqlText[j])) || unicode.IsDigit(rune(sqlText[j])) || sqlText[j] == '_') {
				j++
			}
			if j < len(sqlText) && sqlText[j] == '$' {
				tag := sqlText[i : j+1]
				end := strings.Index(sqlText[j+1:], tag)
				if end < 0 {
					return nil, 0, fmt.Errorf("SQL dollar-quote 未闭合")
				}
				hasContent = true
				i = j + 1 + end + len(tag)
				continue
			}
		}
		if ch == '(' {
			depth++
			hasContent = true
			i++
			continue
		}
		if ch == ')' {
			if depth > 0 {
				depth--
			}
			hasContent = true
			i++
			continue
		}
		if ch == ';' && depth == 0 {
			if hasContent {
				statements++
				hasContent = false
			}
			i++
			continue
		}
		if unicode.IsLetter(rune(ch)) || ch == '_' {
			start := i
			for i < len(sqlText) && (unicode.IsLetter(rune(sqlText[i])) || unicode.IsDigit(rune(sqlText[i])) || sqlText[i] == '_' || sqlText[i] == '$') {
				i++
			}
			tokens = append(tokens, strings.ToUpper(sqlText[start:i]))
			hasContent = true
			continue
		}
		hasContent = true
		i++
	}
	if depth != 0 {
		return nil, 0, fmt.Errorf("SQL 括号未闭合")
	}
	if hasContent {
		statements++
	}
	return tokens, statements, nil
}

func isProductionEnvironment(env string) bool {
	normalized := strings.ToLower(strings.TrimSpace(env))
	return strings.Contains(normalized, "生产") || strings.Contains(normalized, "production") || strings.Contains(normalized, "prod")
}
