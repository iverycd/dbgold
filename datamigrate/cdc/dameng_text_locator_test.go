package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

func TestDamengFullRowTextPredicatesAreExact(t *testing.T) {
	db, _ := newSequenceMock(t)
	for _, typ := range []string{"VARCHAR", "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR"} {
		t.Run(typ, func(t *testing.T) {
			r := damengTestRow(t, db, false, true)
			r.columns[1].DataType = typ
			where, args, err := r.predicate([]int{1}, []any{nil, "a "})
			require.NoError(t, err)
			column, want := `"VALUE"`, "a "
			if typ == "CHAR" || typ == "NCHAR" {
				column, want = `RTRIM("VALUE")`, "a"
			}
			require.Equal(t, "DBMS_LOB.COMPARE(CAST("+column+" AS CLOB), CAST(? AS CLOB))=0", where)
			clob, ok := args[0].(*dm.DmClob)
			require.True(t, ok)
			got, err := clob.ReadString(1, len(want))
			require.NoError(t, err)
			require.Equal(t, want, got)
			r.table.LocatorStrategy = LocatorUniqueKey
			where, args, err = r.predicate([]int{1}, []any{nil, "a "})
			require.NoError(t, err)
			require.Equal(t, `"VALUE"=?`, where, "keyed locators retain their established semantics")
			require.Equal(t, []any{"a "}, args)
		})
	}
}

// Scalar equality in DM can ignore trailing spaces. Keep the table keyless and
// single-column so another predicate cannot hide an incorrect text match.
func TestDamengFullRowTextLocatorIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_CDC_DM_DSN")
	if dsn == "" && os.Getenv("CDC_DAMENG_INTEGRATION") == "1" {
		dsn = os.Getenv("CDC_DAMENG_DSN")
	}
	if dsn == "" {
		t.Skip("set DBGOLD_CDC_DM_DSN to a disposable DM instance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	schema := fmt.Sprintf("CDC_TEXT_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)+" AUTHORIZATION SYSDBA")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, err) })
	a, err := newDamengApplier(ctx, Config{TargetDBType: "dameng", TargetDSN: dsn, TargetSchema: schema, JobID: "exact-text"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	position := uint32(4)
	for _, tc := range []struct {
		name, definition, sourceType string
		fixed                        bool
	}{
		{"VTEXT", "VARCHAR(40)", "VARCHAR", false},
		{"NVTEXT", "NVARCHAR2(40)", "VARCHAR", false},
		{"CTEXT", "CHAR(10)", "CHAR", true},
		{"NCTEXT", "NCHAR(10)", "CHAR", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quoteIdent(schema) + "." + quoteIdent(tc.name)
			_, err = db.ExecContext(ctx, "CREATE TABLE "+q+` ("V" `+tc.definition+")")
			require.NoError(t, err)
			info := &TableInfo{Name: tc.name, Columns: []string{"V"}, ColumnTypes: []string{tc.sourceType}, LocatorStrategy: LocatorFullRow, LocatorColumns: []string{"V"}}
			initial := []any{"a ", "a", "A", "中 ", "中", nil, ""}
			if tc.fixed {
				initial = []any{"a", "A", "中", nil, ""}
			}
			want := map[string]int{}
			key := func(value any) string {
				if value == nil {
					return "<NULL>"
				}
				text := value.(string)
				if tc.fixed {
					text = strings.TrimRight(text, " ")
				}
				return text
			}
			apply := func(action string, before, after any) {
				t.Helper()
				position += 10
				_, err := a.Apply(ctx, []Change{{Action: action, Table: info, Before: []any{before}, After: []any{after}}}, Position{File: "bin.000001", Pos: position})
				require.NoError(t, err, "%s before=%#v after=%#v", action, before, after)
				if action != "insert" {
					want[key(before)]--
					if want[key(before)] == 0 {
						delete(want, key(before))
					}
				}
				if action != "delete" {
					want[key(after)]++
				}
				rows, err := db.QueryContext(ctx, `SELECT "V" FROM `+q)
				require.NoError(t, err)
				got := map[string]int{}
				for rows.Next() {
					var value sql.NullString
					require.NoError(t, rows.Scan(&value))
					if value.Valid {
						got[key(value.String)]++
					} else {
						got[key(nil)]++
					}
				}
				require.NoError(t, closeRows(rows))
				require.Equal(t, want, got, "only the exact source before-row may change")
			}
			for _, value := range initial {
				apply("insert", nil, value)
			}
			if tc.fixed {
				apply("update", "a", "short")
				apply("update", "short   ", "shorter")
			} else {
				apply("delete", "a", nil)
				apply("update", "a ", "A ")
			}
			apply("update", "中", "中改")
			apply("delete", "A", nil)
			apply("delete", "", nil)
			apply("update", nil, "")
			apply("delete", "", nil)
		})
	}
}
