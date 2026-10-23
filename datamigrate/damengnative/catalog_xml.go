package damengnative

import (
	"fmt"
	"strings"
)

// DM8 represents XMLTYPE as a CLOB/TEXT column with native scale 8. Its
// DBMS_METADATA.GET_DDL('TABLE') can omit that XML marker and emit plain TEXT.
// Keep the raw TEXT value codec; XMLTYPE construction/serialization can alter
// whitespace in already stored XML, whereas direct CLOB bindings preserve it.
func nativeXMLColumn(column Column) bool {
	return column.TypeSchema == "" && column.Scale == 8 && (strings.EqualFold(strings.TrimSpace(column.DataType), "TEXT") || strings.EqualFold(strings.TrimSpace(column.DataType), "CLOB"))
}

// RestoreNativeXMLTableDDL applies the same native attribute proof to frozen
// physical partition/buffer definitions whose column inventory is already
// known. Missing or mismatched columns fail instead of silently losing XML.
func RestoreNativeXMLTableDDL(ddl string, columns []Column) (string, error) {
	wanted := map[string]bool{}
	for _, column := range columns {
		if nativeXMLColumn(column) {
			wanted[column.Name] = true
		}
	}
	if len(wanted) == 0 {
		return ddl, nil
	}
	restored, found, err := nativeXMLTableDDL(ddl, wanted)
	if err != nil {
		return "", err
	}
	if len(found) != len(wanted) {
		return "", fmt.Errorf("XMLTYPE 原生目录属性无法与完整承载表列声明对应")
	}
	return restored, nil
}

func restoreNativeXMLAttributes(object *Object, table *Table) error {
	wanted := map[string]bool{}
	for _, column := range table.Columns {
		if nativeXMLColumn(column) {
			wanted[column.Name] = true
		}
	}
	if len(wanted) == 0 || len(object.Units) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for i, unit := range object.Units {
		if unit.Phase != "before_data" {
			continue
		}
		ddl, found, err := nativeXMLTableDDL(unit.SQL, wanted)
		if err != nil {
			return err
		}
		object.Units[i].SQL = ddl
		for name := range found {
			seen[name] = true
		}
	}
	if len(seen) != len(wanted) {
		return fmt.Errorf("表 %s 的 XMLTYPE 原生目录属性无法与完整列声明对应", table.Name)
	}
	return nil
}

// Only the datatype token of a catalog-proven XML column is replaced. Quoted
// names, literals, comments, constraints, defaults and other execution units
// retain their original bytes.
func nativeXMLTableDDL(ddl string, wanted map[string]bool) (string, map[string]bool, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return "", nil, err
	}
	found := map[string]bool{}
	word := func(i int, text string) bool {
		return i < len(ts) && ts[i].kind == 'i' && strings.EqualFold(ts[i].value, text)
	}
	if !word(0, "CREATE") {
		return ddl, found, nil
	}
	i := 1
	for ; i < len(ts) && !word(i, "TABLE"); i++ {
		if !word(i, "GLOBAL") && !word(i, "LOCAL") && !word(i, "TEMPORARY") && !word(i, "HUGE") && !word(i, "EXTERNAL") {
			return ddl, found, nil
		}
	}
	if i == len(ts) {
		return ddl, found, nil
	}
	i++
	if word(i, "IF") && word(i+1, "NOT") && word(i+2, "EXISTS") {
		i += 3
	}
	if i >= len(ts) || ts[i].kind != 'q' && ts[i].kind != 'i' {
		return ddl, found, nil
	}
	i++
	if i+1 < len(ts) && ts[i].value == "." && (ts[i+1].kind == 'q' || ts[i+1].kind == 'i') {
		i += 2
	}
	if i >= len(ts) || ts[i].value != "(" {
		return ddl, found, nil
	}
	var replacements []sqlToken
	inspect := func(start, end int) error {
		if start+1 >= end || ts[start].kind != 'q' && ts[start].kind != 'i' {
			return nil
		}
		name := ts[start].value
		if ts[start].kind == 'i' {
			name = strings.ToUpper(name)
			switch name {
			case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "NOT", "CLUSTER":
				return nil
			}
		}
		if !wanted[name] {
			return nil
		}
		typeToken := ts[start+1]
		if typeToken.kind != 'i' || !strings.EqualFold(typeToken.value, "TEXT") && !strings.EqualFold(typeToken.value, "CLOB") && !strings.EqualFold(typeToken.value, "XMLTYPE") {
			return fmt.Errorf("列 %s 的 XMLTYPE 目录属性与原生声明不同", name)
		}
		found[name] = true
		if !strings.EqualFold(typeToken.value, "XMLTYPE") {
			replacements = append(replacements, typeToken)
		}
		return nil
	}
	depth, start := 1, i+1
	closed := false
	for j := i + 1; j < len(ts); j++ {
		if ts[j].kind != 'p' {
			continue
		}
		switch ts[j].value {
		case "(":
			depth++
		case ",":
			if depth == 1 {
				if err = inspect(start, j); err != nil {
					return "", nil, err
				}
				start = j + 1
			}
		case ")":
			depth--
			if depth == 0 {
				if err = inspect(start, j); err != nil {
					return "", nil, err
				}
				closed = true
			}
		}
		if closed {
			break
		}
	}
	if !closed {
		return "", nil, fmt.Errorf("XMLTYPE 所属原生表列声明不完整")
	}
	for j := len(replacements) - 1; j >= 0; j-- {
		t := replacements[j]
		ddl = ddl[:t.start] + "XMLTYPE" + ddl[t.end:]
	}
	return ddl, found, nil
}
