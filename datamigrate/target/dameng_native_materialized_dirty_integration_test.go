package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeMaterializedIdentityTouchIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("自增基表原生物化状态验收需要独立目标 DSN/Schema 和 DAMENG_DM_TEST_ALLOW_DDL=1")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, step := range []int64{2, -3} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("step%d/interrupted%v", step, interrupted), func(t *testing.T) {
				base := fmt.Sprintf("TouchB%X", time.Now().UnixNano())
				view := fmt.Sprintf("TouchV%X", time.Now().UnixNano())
				q, mv := damengnative.Qualified(w.schema, base), damengnative.Qualified(w.schema, view)
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
					defer stop()
					_, _ = w.db.ExecContext(cleanup, "DROP MATERIALIZED VIEW "+mv)
					_, _ = w.db.ExecContext(cleanup, "DROP TABLE "+q+" CASCADE CONSTRAINTS")
				})
				exec := func(statement string) { _, err := w.db.ExecContext(ctx, statement); require.NoError(t, err) }
				exec(fmt.Sprintf("CREATE TABLE %s (\"Id\" BIGINT IDENTITY(100,%d) PRIMARY KEY)", q, step))
				exec("INSERT INTO " + q + " DEFAULT VALUES")
				exec("INSERT INTO " + q + " DEFAULT VALUES")
				next := 100 + 2*step
				drop := "ALTER TABLE " + q + " DROP IDENTITY"
				add := fmt.Sprintf("ALTER TABLE %s ADD COLUMN \"Id\" IDENTITY(%d,%d)", q, next, step)
				// Match the native import's already-frozen continuation seed.
				exec(drop)
				exec(add)
				exec("CREATE MATERIALIZED VIEW LOG ON " + q + " WITH PRIMARY KEY")
				exec("CREATE MATERIALIZED VIEW " + mv + " REFRESH FAST WITH PRIMARY KEY ON DEMAND AS SELECT \"Id\" FROM " + q)
				readRows := func() map[int64]string {
					rows, err := w.db.QueryContext(ctx, "SELECT \"Id\",ROWIDTOCHAR(ROWID) FROM "+q)
					require.NoError(t, err)
					defer rows.Close()
					got := map[int64]string{}
					for rows.Next() {
						var id int64
						var rowid string
						require.NoError(t, rows.Scan(&id, &rowid))
						got[id] = rowid
					}
					require.NoError(t, rows.Err())
					return got
				}
				before := readRows()
				if interrupted {
					exec(drop) // previous DROP committed, its ADD response was lost
				}
				conn, err := w.db.Conn(ctx)
				require.NoError(t, err)
				touch := "UPDATE " + q + " SET \"Id\"=\"Id\" WHERE ROWID=(SELECT ROWID FROM " + q + " WHERE ROWNUM=1)"
				err = nativeHistoryTouch(ctx, conn, w.schema, []string{drop, touch, add})
				require.NoError(t, conn.Close())
				require.NoError(t, err)
				require.Equal(t, before, readRows(), "原值和全部已有 ROWID 必须保持一致")
				var seed, increment, current int64
				require.NoError(t, w.db.QueryRowContext(ctx, `SELECT IDENT_SEED(?),IDENT_INCR(?),IDENT_CURRENT(?) FROM DUAL`, q, q, q).Scan(&seed, &increment, &current))
				require.Equal(t, next, seed)
				require.Equal(t, step, increment)
				require.Equal(t, next, current)
				require.Eventually(t, func() bool {
					var state string
					err := w.db.QueryRowContext(ctx, `SELECT STALENESS FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, w.schema, view).Scan(&state)
					return err == nil && state == "NEEDS_COMPILE"
				}, 3*time.Second, 20*time.Millisecond)
				exec("INSERT INTO " + q + " DEFAULT VALUES")
				require.NoError(t, w.db.QueryRowContext(ctx, "SELECT IDENT_CURRENT(?) FROM DUAL", q).Scan(&current))
				require.Equal(t, next, current, "不能消耗尚未使用的续号种子")
				exec("REFRESH MATERIALIZED VIEW " + mv + " FAST")
				var count int
				require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mv).Scan(&count))
				require.Equal(t, 3, count)
			})
		}
	}
}
