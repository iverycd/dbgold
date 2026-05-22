package datamigrate

import (
	"path/filepath"
	"strings"
)

// FilterTables 根据 mode 和 filter 字符串过滤表名列表。
// mode: "all" | "include" | "exclude"
// filter: 逗号分隔的表名或通配符（支持 * 匹配任意字符串）
func FilterTables(tables []string, mode, filter string) []string {
	if mode == "all" {
		return tables
	}
	patterns := parsePatterns(filter)
	result := make([]string, 0, len(tables))
	for _, t := range tables {
		matched := matchesAny(t, patterns)
		if mode == "include" && matched {
			result = append(result, t)
		} else if mode == "exclude" && !matched {
			result = append(result, t)
		}
	}
	return result
}

func parsePatterns(filter string) []string {
	if filter == "" {
		return nil
	}
	parts := strings.Split(filter, ",")
	patterns := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}
