package schema

import (
	"regexp"
	"strings"
)

var (
	reCreateTable    = regexp.MustCompile("(?i)CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?(\\w+)`?")
	reCreateView     = regexp.MustCompile("(?i)CREATE\\s+(?:OR\\s+REPLACE\\s+)?VIEW\\s+`?(\\w+)`?")
	reCreateIndex    = regexp.MustCompile("(?i)CREATE\\s+(?:UNIQUE\\s+)?INDEX\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?(\\w+)`?")
	reCreateTrigger  = regexp.MustCompile("(?i)CREATE\\s+(?:OR\\s+REPLACE\\s+)?TRIGGER\\s+`?(\\w+)`?")
	reCreateSequence = regexp.MustCompile("(?i)CREATE\\s+SEQUENCE\\s+`?(\\w+)`?")
)

// ParseDDL 从 DDL 文本解析出 FullSchema 的结构信息（只提取对象名称和类型）
func ParseDDL(ddl string) (*FullSchema, error) {
	s := &FullSchema{}

	statements := splitStatements(ddl)
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		upper := strings.ToUpper(stmt)
		switch {
		case strings.Contains(upper, "CREATE TABLE"):
			if m := reCreateTable.FindStringSubmatch(stmt); len(m) > 1 {
				t := Table{Name: m[1]}
				t.Columns = parseColumns(stmt)
				s.Tables = append(s.Tables, t)
			}
		case strings.Contains(upper, "CREATE") && strings.Contains(upper, "VIEW"):
			if m := reCreateView.FindStringSubmatch(stmt); len(m) > 1 {
				s.Views = append(s.Views, View{Name: m[1], Def: stmt})
			}
		case strings.Contains(upper, "CREATE TRIGGER"):
			if m := reCreateTrigger.FindStringSubmatch(stmt); len(m) > 1 {
				s.Triggers = append(s.Triggers, Trigger{Name: m[1], Body: stmt})
			}
		case strings.Contains(upper, "CREATE SEQUENCE"):
			if m := reCreateSequence.FindStringSubmatch(stmt); len(m) > 1 {
				s.Sequences = append(s.Sequences, Sequence{Name: m[1], Start: 1, Increment: 1})
			}
		case strings.Contains(upper, "CREATE") && strings.Contains(upper, "INDEX"):
			if m := reCreateIndex.FindStringSubmatch(stmt); len(m) > 1 {
				_ = m[1] // index objects tracked per-table elsewhere
			}
		}
	}
	return s, nil
}

func splitStatements(ddl string) []string {
	return strings.Split(ddl, ";")
}

var reColLine = regexp.MustCompile("(?i)^\\s*`?(\\w+)`?\\s+(\\S+)")

func parseColumns(createStmt string) []Column {
	start := strings.Index(createStmt, "(")
	end := strings.LastIndex(createStmt, ")")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	body := createStmt[start+1 : end]
	var cols []Column
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, ","))
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "PRIMARY") || strings.HasPrefix(upper, "KEY") ||
			strings.HasPrefix(upper, "INDEX") || strings.HasPrefix(upper, "UNIQUE") ||
			strings.HasPrefix(upper, "CONSTRAINT") || strings.HasPrefix(upper, "FOREIGN") ||
			strings.HasPrefix(upper, "CHECK") || line == "" {
			continue
		}
		if m := reColLine.FindStringSubmatch(line); len(m) > 2 {
			col := Column{
				Name:     m[1],
				Type:     m[2],
				Nullable: !strings.Contains(upper, "NOT NULL"),
			}
			if strings.Contains(upper, "AUTO_INCREMENT") || strings.Contains(upper, "IDENTITY") || strings.Contains(upper, "SERIAL") {
				col.AutoIncrement = true
			}
			cols = append(cols, col)
		}
	}
	return cols
}
