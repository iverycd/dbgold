package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"dbgold/datamigrate/valueconv"
	"gitee.com/chunanyong/dm"
)

type damengRow struct {
	applier  *DamengApplier
	table    *TableInfo
	columns  []targetColumnMetadata
	identity []int
}

func newDamengRow(a *DamengApplier, table *TableInfo, target *targetTableMetadata) (*damengRow, error) {
	if len(table.Columns) == 0 || len(table.ColumnTypes) != len(table.Columns) {
		return nil, fmt.Errorf("表 %s 源列类型不完整", table.Name)
	}
	r := &damengRow{applier: a, table: table}
	byName := map[string]targetColumnMetadata{}
	for _, c := range target.Columns {
		byName[c.Name] = c
	}
	for i, name := range table.Columns {
		c, exists := byName[a.name(name)]
		if !exists {
			return nil, fmt.Errorf("目标表 %s 缺少列 %s", table.Name, a.name(name))
		}
		r.columns = append(r.columns, c)
		if c.Identity == "YES" {
			r.identity = append(r.identity, i)
		}
	}
	return r, nil
}

func (r *damengRow) qualified() string       { return r.applier.qualified(r.table.Name) }
func (r *damengRow) column(index int) string { return quoteIdent(r.columns[index].Name) }
func (r *damengRow) identityChanged(before, after []any) bool {
	for _, index := range r.identity {
		if !reflect.DeepEqual(before[index], after[index]) {
			return true
		}
	}
	return false
}
func (r *damengRow) bind(index int, value any) (string, any, error) {
	typ := strings.ToUpper(r.columns[index].DataType)
	switch typ {
	case "CLOB", "NCLOB", "TEXT", "LONG", "LONGVARCHAR":
		if value == nil {
			return "CAST(? AS CLOB)", nil, nil
		}
		switch v := value.(type) {
		case string:
			return "CAST(? AS CLOB)", dm.NewClob(v), nil
		case []byte:
			return "CAST(? AS CLOB)", dm.NewClob(string(v)), nil
		default:
			return "", nil, fmt.Errorf("列 %s 的 CLOB 值类型不支持: %T", r.table.Columns[index], value)
		}
	case "BLOB", "IMAGE":
		if value == nil {
			return "CAST(? AS BLOB)", nil, nil
		}
		switch v := value.(type) {
		case []byte:
			return "CAST(? AS BLOB)", dm.NewBlob(v), nil
		case string:
			return "CAST(? AS BLOB)", dm.NewBlob([]byte(v)), nil
		default:
			return "", nil, fmt.Errorf("列 %s 的 BLOB 值类型不支持: %T", r.table.Columns[index], value)
		}
	default:
		return valueconv.MySQLDamengPlaceholder(r.table.ColumnTypes[index]), value, nil
	}
}
func (r *damengRow) predicate(indexes []int, row []any) (string, []any, error) {
	var predicates []string
	var args []any
	for _, index := range indexes {
		if index < 0 || index >= len(row) {
			return "", nil, fmt.Errorf("表 %s 定位列序号越界", r.table.Name)
		}
		if row[index] == nil {
			predicates = append(predicates, r.column(index)+" IS NULL")
			continue
		}
		mark, value, err := r.bind(index, row[index])
		if err != nil {
			return "", nil, err
		}
		if column, fixed, text := r.fullRowTextColumn(index); text {
			// DM VARCHAR equality can ignore trailing spaces and depends on the
			// target collation. A full-row locator must compare the complete value.
			// Fixed CHAR/NCHAR padding is not part of the source reader's value.
			if mark != "?" {
				// Keep typed source conversions when an existing target stores a
				// date/number in a character column instead of the default mapping.
				mark = "CAST(" + mark + " AS CLOB)"
			} else {
				switch v := value.(type) {
				case string:
					if fixed {
						v = strings.TrimRight(v, " ")
					}
					value = dm.NewClob(v)
					mark = "CAST(? AS CLOB)"
				case []byte:
					vtext := string(v)
					if fixed {
						vtext = strings.TrimRight(vtext, " ")
					}
					value = dm.NewClob(vtext)
					mark = "CAST(? AS CLOB)"
				default:
					mark = "CAST(" + mark + " AS CLOB)"
				}
			}
			predicates = append(predicates, "DBMS_LOB.COMPARE(CAST("+column+" AS CLOB), "+mark+")=0")
		} else if strings.Contains(mark, " AS CLOB)") || strings.Contains(mark, " AS BLOB)") {
			predicates = append(predicates, "DBMS_LOB.COMPARE("+r.column(index)+", "+mark+")=0")
		} else {
			predicates = append(predicates, r.column(index)+"="+mark)
		}
		args = append(args, value)
	}
	if len(predicates) == 0 {
		return "", nil, fmt.Errorf("表 %s 没有定位列", r.table.Name)
	}
	return strings.Join(predicates, " AND "), args, nil
}

func (r *damengRow) fullRowTextColumn(index int) (column string, fixed, text bool) {
	if r.table.LocatorStrategy != LocatorFullRow {
		return "", false, false
	}
	switch strings.ToUpper(r.columns[index].DataType) {
	case "CHAR", "CHARACTER", "NCHAR", "NATIONAL CHARACTER":
		return "RTRIM(" + r.column(index) + ")", true, true
	case "VARCHAR", "VARCHAR2", "NVARCHAR", "NVARCHAR2", "CHARACTER VARYING", "NATIONAL CHARACTER VARYING":
		return r.column(index), false, true
	default:
		return "", false, false
	}
}

func (r *damengRow) allIndexes() []int {
	indexes := make([]int, len(r.columns))
	for i := range indexes {
		indexes[i] = i
	}
	return indexes
}
func (r *damengRow) insert(ctx context.Context, tx *sql.Tx, row []any, setIdentity func(string) error) error {
	if len(r.identity) > 0 {
		if err := setIdentity(r.qualified()); err != nil {
			return err
		}
	}
	cols, marks := make([]string, len(row)), make([]string, len(row))
	args := make([]any, len(row))
	for i, value := range row {
		cols[i] = r.column(i)
		var err error
		marks[i], args[i], err = r.bind(i, value)
		if err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+r.qualified()+" ("+strings.Join(cols, ",")+") VALUES ("+strings.Join(marks, ",")+")", args...)
	return err
}
func (r *damengRow) delete(ctx context.Context, tx *sql.Tx, before []any, full bool) error {
	indexes := r.allIndexes()
	if !full {
		var err error
		indexes, err = locatorColumnIndexes(r.table)
		if err != nil {
			return err
		}
	}
	where, args, err := r.predicate(indexes, before)
	if err != nil {
		return err
	}
	if full {
		where = "ROWID=(SELECT ROWID FROM " + r.qualified() + " WHERE " + where + " AND ROWNUM=1)"
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM "+r.qualified()+" WHERE "+where, args...)
	if err != nil {
		return err
	}
	if full {
		return requireOneFullRow(r.table, "delete", before, result)
	}
	return nil
}
func (r *damengRow) update(ctx context.Context, tx *sql.Tx, before, after []any, full bool) (int64, error) {
	identity := map[int]bool{}
	for _, i := range r.identity {
		identity[i] = true
	}
	locator := map[int]bool{}
	indexes := r.allIndexes()
	if !full {
		var err error
		indexes, err = locatorColumnIndexes(r.table)
		if err != nil {
			return 0, err
		}
		for _, i := range indexes {
			locator[i] = true
		}
		// An upsert must not silently preserve a different identity value.
		for _, i := range r.identity {
			if !locator[i] {
				indexes = append(indexes, i)
			}
		}
	}
	where, whereArgs, err := r.predicate(indexes, before)
	if err != nil {
		return 0, err
	}
	var sets []string
	var args []any
	for i, value := range after {
		if identity[i] || (!full && locator[i]) {
			continue
		}
		mark, v, e := r.bind(i, value)
		if e != nil {
			return 0, e
		}
		sets = append(sets, r.column(i)+"="+mark)
		args = append(args, v)
	}
	if len(sets) == 0 {
		var count int64
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+r.qualified()+" WHERE "+where+" AND ROWNUM=1", whereArgs...).Scan(&count)
		return count, err
	}
	if full {
		where = "ROWID=(SELECT ROWID FROM " + r.qualified() + " WHERE " + where + " AND ROWNUM=1)"
	}
	args = append(args, whereArgs...)
	result, err := tx.ExecContext(ctx, "UPDATE "+r.qualified()+" SET "+strings.Join(sets, ",")+" WHERE "+where, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
func (r *damengRow) upsert(ctx context.Context, tx *sql.Tx, row []any, setIdentity func(string) error) error {
	if r.table.LocatorStrategy == LocatorFullRow {
		return r.insert(ctx, tx, row, setIdentity)
	}
	n, err := r.update(ctx, tx, row, row, false)
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	if n > 1 {
		return fmt.Errorf("表 %s 唯一键定位影响 %d 行", r.table.Name, n)
	}
	// A matching locator with a different identity cannot be updated in DM.
	if len(r.identity) > 0 {
		if err = r.delete(ctx, tx, row, false); err != nil {
			return err
		}
	}
	return r.insert(ctx, tx, row, setIdentity)
}
func (r *damengRow) apply(ctx context.Context, tx *sql.Tx, ch Change, setIdentity func(string) error) error {
	var before, after []any
	var err error
	if ch.Action != "insert" {
		before, err = convertDamengBinlogRow(r.table, ch.Before)
		if err != nil {
			return err
		}
	}
	if ch.Action != "delete" {
		after, err = convertDamengBinlogRow(r.table, ch.After)
		if err != nil {
			return err
		}
	}
	full := r.table.LocatorStrategy == LocatorFullRow
	switch ch.Action {
	case "insert":
		return r.upsert(ctx, tx, after, setIdentity)
	case "delete":
		return r.delete(ctx, tx, before, full)
	case "update":
		if full {
			if r.identityChanged(before, after) {
				if err = r.delete(ctx, tx, before, true); err != nil {
					return err
				}
				return r.insert(ctx, tx, after, setIdentity)
			}
			n, e := r.update(ctx, tx, before, after, true)
			if e != nil {
				return e
			}
			return requireOneFullRow(r.table, "update", ch.Before, damengRowsAffected(n))
		}
		if locatorChanged(r.table, before, after) || r.identityChanged(before, after) {
			if err = r.delete(ctx, tx, before, false); err != nil {
				return err
			}
		}
		return r.upsert(ctx, tx, after, setIdentity)
	}
	return fmt.Errorf("未知 CDC 操作: %s", ch.Action)
}

type damengRowsAffected int64

func (n damengRowsAffected) LastInsertId() (int64, error) {
	return 0, fmt.Errorf("不支持 LastInsertId")
}
func (n damengRowsAffected) RowsAffected() (int64, error) { return int64(n), nil }
