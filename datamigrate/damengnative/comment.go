package damengnative

import (
	"fmt"
	"strings"
)

// CommentStatement contains the catalog identifiers and text of one COMMENT.
type CommentStatement struct {
	Schema, Table, Column, Text string
}

func ParseCommentSQL(sql, defaultSchema string) (CommentStatement, error) {
	var cm CommentStatement
	// Only ordinary SQL comments are trivia in a repair statement. Inspect
	// their actual tokens so markers inside the comment text remain literal.
	ts, err := scanNativeTokens(sql, nil, func(block string) error {
		body := strings.TrimSpace(block)
		if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "M!") || strings.HasPrefix(body, "m!") || strings.Contains(block, "/*") {
			return fmt.Errorf("达梦注释修复不支持可执行或嵌套的 SQL 注释")
		}
		return nil
	})
	if err != nil {
		return cm, err
	}
	word := func(i int, s string) bool {
		return i < len(ts) && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, s)
	}
	punct := func(i int, s string) bool {
		return i < len(ts) && ts[i].kind == 'p' && ts[i].value == s
	}
	column := word(2, "COLUMN")
	if !word(0, "COMMENT") || !word(1, "ON") || (!word(2, "TABLE") && !column) {
		return cm, fmt.Errorf("达梦注释修复仅支持 COMMENT ON TABLE 或 COMMENT ON COLUMN")
	}
	var names []string
	i := 3
	for {
		if i >= len(ts) || ts[i].value == "" || (ts[i].kind != 'i' && ts[i].kind != 'q') {
			return cm, fmt.Errorf("达梦注释缺少有效的目标标识符")
		}
		name := ts[i].value
		if ts[i].kind == 'i' {
			name = strings.ToUpper(name)
		}
		names = append(names, name)
		i++
		if !punct(i, ".") {
			break
		}
		i++
	}
	if column {
		cm.Column = names[len(names)-1]
		names = names[:len(names)-1]
	}
	switch len(names) {
	case 1:
		cm.Schema, cm.Table = defaultSchema, names[0]
	case 2:
		cm.Schema, cm.Table = names[0], names[1]
	default:
		return cm, fmt.Errorf("达梦注释须指定所属表，可使用模式名限定")
	}
	if !word(i, "IS") || i+1 >= len(ts) {
		return cm, fmt.Errorf("达梦注释缺少 IS 和注释正文")
	}
	i++
	if word(i, "N") && i+1 < len(ts) && ts[i].end == ts[i+1].start && ts[i+1].kind == 's' {
		i++
	}
	if ts[i].kind == 's' {
		cm.Text = ts[i].value
	} else if !word(i, "NULL") {
		return cm, fmt.Errorf("达梦注释正文须为字符串或 NULL")
	}
	i++
	if punct(i, ";") {
		i++
	}
	if i != len(ts) {
		return cm, fmt.Errorf("每个执行单元只能包含一条完整的达梦注释语句")
	}
	return cm, nil
}
