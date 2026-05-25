package migrate_test

import "strings"

func strPtr(s string) *string { return &s }

func findSQL(sqls []string, keyword string) string {
	for _, s := range sqls {
		if strings.Contains(s, keyword) {
			return s
		}
	}
	return ""
}
