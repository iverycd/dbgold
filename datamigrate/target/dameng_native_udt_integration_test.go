package target

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

// The source and destination tables deliberately share one test-only schema:
// this isolates the driver's source decoding and destination wire encoding.
// Production migration's separate-schema guard is tested by the orchestrator.
func TestDamengNativeIntegrationNestedUDTExactRoundtrip(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("实库未验收：需要 DAMENG_DM_TEST_TARGET_DSN / TARGET_SCHEMA 与 DAMENG_DM_TEST_ALLOW_DDL=1，仅用于独立空测试 schema")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var count int
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=?`, w.schema).Scan(&count))
	require.Zero(t, count, "测试 schema 必须最初为空")
	prefix := fmt.Sprintf("DNUT%X", time.Now().UnixNano())
	inner, bag, outer, src, dst := prefix+`Inner.Type"Exact`, prefix+"ArrayExact", prefix+"OuterExact", prefix+"S", prefix+"T"
	matrix := prefix + "NestedArrays"
	q := func(name string) string { return damengnative.Qualified(w.schema, name) }
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, table := range []string{dst, src} {
			_ = w.ExecuteDamengNative(cleanup, "DROP TABLE "+q(table)+" CASCADE CONSTRAINTS")
		}
		for _, typ := range []string{outer, matrix, bag, inner} {
			_ = w.ExecuteDamengNative(cleanup, "DROP TYPE "+q(typ))
		}
	})
	for _, ddl := range []string{
		"CREATE TYPE " + q(inner) + ` AS OBJECT ("Payload" BLOB,"Body" CLOB,"Amount" DECIMAL(38,19))`,
		"CREATE TYPE " + q(bag) + " AS VARRAY(5) OF " + q(inner),
		"CREATE TYPE " + q(matrix) + " AS VARRAY(5) OF " + q(bag),
		"CREATE TYPE " + q(outer) + ` AS OBJECT ("Items" ` + q(bag) + `,"Payload" BLOB,"Body" CLOB,"Amount" DECIMAL(38,19))`,
		"CREATE TABLE " + q(src) + ` ("Id" INT,"Value" ` + q(outer) + `,"Matrix" ` + q(matrix) + ")",
		"CREATE TABLE " + q(dst) + ` ("Id" INT,"Value" ` + q(outer) + `,"Matrix" ` + q(matrix) + ")",
	} {
		require.NoError(t, w.ExecuteDamengNative(ctx, ddl))
	}
	negative := "-1234567890123456789.1234567890123456789"
	innerValue := q(inner) + `(HEXTORAW('0001FF00'),TO_CLOB('中文AbC😀'),` + negative + ")"
	emptyInner := q(inner) + "(EMPTY_BLOB(),EMPTY_CLOB(),NULL)"
	for _, statement := range []string{
		"INSERT INTO " + q(src) + " VALUES (1," + q(outer) + "(" + q(bag) + "(" + innerValue + ",NULL," + emptyInner + "),HEXTORAW('00AAFF'),TO_CLOB('对象内大字段中文😀')," + negative + ")," + q(matrix) + "(" + q(bag) + "(" + innerValue + ",NULL),NULL," + q(bag) + "()))",
		"INSERT INTO " + q(src) + " VALUES (2," + q(outer) + "(" + q(bag) + "(),EMPTY_BLOB(),EMPTY_CLOB(),NULL)," + q(matrix) + "())",
		"INSERT INTO " + q(src) + " VALUES (3," + q(outer) + "(NULL,NULL,NULL,NULL)," + q(matrix) + "(NULL))",
		"INSERT INTO " + q(src) + " VALUES (4,NULL,NULL)",
	} {
		_, err = w.db.ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	read := func(table string) ([][]any, []any) {
		rows, err := w.db.QueryContext(ctx, `SELECT "Id","Value","Matrix" FROM `+q(table)+` ORDER BY "Id"`)
		require.NoError(t, err)
		defer rows.Close()
		var result [][]any
		var canonical []any
		for rows.Next() {
			var id, value, matrixValue any
			require.NoError(t, rows.Scan(&id, &value, &matrixValue))
			result = append(result, []any{id, value, matrixValue})
			canonical = append(canonical, []any{id, udtFixtureValue(t, value), udtFixtureValue(t, matrixValue)})
		}
		require.NoError(t, rows.Err(), "读取 %s 时已成功解码 %d 行", table, len(result))
		return result, canonical
	}
	values, want := read(src)
	table := damengnative.Table{Name: dst, Schema: w.schema, Columns: []damengnative.Column{{Name: "Id", DataType: "INT", SQLType: "INT", Writable: true}, {Name: "Value", DataType: outer, SQLType: q(outer), TypeSchema: w.schema, TypeName: outer, Writable: true}, {Name: "Matrix", DataType: matrix, SQLType: q(matrix), TypeSchema: w.schema, TypeName: matrix, Writable: true}}}
	for _, row := range values {
		require.NoError(t, ValidateDamengNativeValue(table.Columns[1], row[1]))
		require.NoError(t, ValidateDamengNativeValue(table.Columns[2], row[2]))
	}
	require.NoError(t, w.CopyDamengNativeBatch(ctx, table, values))
	_, got := read(dst)
	require.Equal(t, want, got)
}

func udtFixtureValue(t *testing.T, value any) any {
	t.Helper()
	if value != nil {
		rv := reflect.ValueOf(value)
		if rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil
		}
	}
	switch v := value.(type) {
	case nil:
		return nil
	case *dm.DmDecimal:
		return v.String()
	case *dm.DmBlob:
		if !v.Valid {
			return nil
		}
		length, err := v.GetLength()
		require.NoError(t, err)
		data := make([]byte, int(length))
		if length > 0 {
			n, err := v.ReadAt(1, data)
			require.NoError(t, err)
			require.Equal(t, len(data), n)
		}
		return []any{"blob", data}
	case *dm.DmClob:
		if !v.Valid {
			return nil
		}
		length, err := v.GetLength()
		require.NoError(t, err)
		text := ""
		if length > 0 {
			text, err = v.ReadString(1, int(length))
			require.NoError(t, err)
		}
		return []any{"clob", text}
	case *dm.DmStruct:
		if !v.Valid {
			return nil
		}
		attrs, err := v.GetAttributes()
		require.NoError(t, err)
		result := []any{"struct"}
		for _, a := range attrs {
			result = append(result, udtFixtureValue(t, a))
		}
		return result
	case *dm.DmArray:
		if !v.Valid {
			return nil
		}
		attrs, err := v.GetArray()
		require.NoError(t, err)
		result := []any{"array"}
		if attrs != nil {
			rv := reflect.ValueOf(attrs)
			for i := 0; i < rv.Len(); i++ {
				result = append(result, udtFixtureValue(t, rv.Index(i).Interface()))
			}
		}
		return result
	default:
		return value
	}
}
