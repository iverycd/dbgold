package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// A deferred MV must retain its uninitialized snapshot and native state even
// when a sibling sharing its log requires the complete stale-history recipe.
func TestDamengNativeDeferredMaterializedIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("延迟构建快照验收需要专用预建空 Schema、SOURCE/TARGET_DSN/SCHEMA 及 DAMENG_NATIVE_ALLOW_DDL=1、DAMENG_NATIVE_ALLOW_MV_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var n int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&n))
		require.Zero(t, n, "必须使用初始为空的专用 Schema")
	}
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "shared_history"}[shared], func(t *testing.T) {
			sq := func(n string) string { return damengnative.Qualified(srcSchema, n) }
			dq := func(n string) string { return damengnative.Qualified(dstSchema, n) }
			var frozen *damengnative.Preflight
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				if frozen != nil {
					ordered := damengnative.Order(frozen.Objects)
					for i := len(ordered) - 1; i >= 0; i-- {
						if !ordered[i].Managed && ordered[i].DropSQL != "" {
							_, _ = dst.ExecContext(clean, ordered[i].DropSQL)
						}
					}
				}
				_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq("Deferred"))
				_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq("Historical"))
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq("Base")+" CASCADE CONSTRAINTS")
			}()
			exec := func(db *sql.DB, q string) { _, e := db.ExecContext(ctx, q); require.NoError(t, e, q) }
			exec(src, "CREATE TABLE "+sq("Base")+` ("Id" INT PRIMARY KEY,"Value" INT)`)
			exec(src, "INSERT INTO "+sq("Base")+" VALUES(1,10)")
			exec(src, "CREATE MATERIALIZED VIEW LOG ON "+sq("Base")+" WITH PRIMARY KEY")
			if shared {
				exec(src, "CREATE MATERIALIZED VIEW "+sq("Historical")+" REFRESH FAST WITH PRIMARY KEY ON DEMAND AS SELECT \"Id\",\"Value\" FROM "+sq("Base"))
				// Keep this server's second-resolution log clock beyond the fresh
				// sibling baseline before creating a pending source change.
				time.Sleep(1200 * time.Millisecond)
				exec(src, "UPDATE "+sq("Base")+` SET "Value"=11 WHERE "Id"=1`)
			}
			exec(src, "CREATE MATERIALIZED VIEW "+sq("Deferred")+" BUILD DEFERRED REFRESH FAST WITH PRIMARY KEY ON DEMAND AS SELECT \"Id\",\"Value\" FROM "+sq("Base"))
			state := func(db *sql.DB, schema, name string) (string, string) {
				var status, last string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT STALENESS,TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, schema, name).Scan(&status, &last))
				return status, last
			}
			wantState, wantLast := state(src, srcSchema, "Deferred")
			require.Equal(t, "UNUSEABLE", wantState)
			r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, MaxParallel: 2})
			frozen = m.PreflightDamengNative(ctx)
			require.True(t, frozen.Ready, "%+v", frozen.Issues)
			require.Equal(t, shared, nativeHasMaterializedHistory(frozen.Objects))
			report := m.Run(ctx)
			if report.HasFailures() {
				b, _ := json.Marshal(report)
				t.Logf("deferred report: %s", b)
			}
			require.False(t, report.HasFailures())
			gotState, gotLast := state(dst, dstSchema, "Deferred")
			require.Equal(t, wantState, gotState, "未构建状态不能被初始化或陈旧状态替代")
			require.Equal(t, wantLast, gotLast, "未构建的原生水位必须保留")
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("Deferred")).Scan(&count))
			require.Zero(t, count, "不能用查询基表填充 BUILD DEFERRED 的空原始快照")
			if shared {
				var value int
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT "Value" FROM `+dq("Historical")+` WHERE "Id"=1`).Scan(&value))
				require.Equal(t, 10, value)
				exec(dst, "REFRESH MATERIALIZED VIEW "+dq("Historical")+" FAST")
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT "Value" FROM `+dq("Historical")+` WHERE "Id"=1`).Scan(&value))
				require.Equal(t, 11, value, "共享日志中待消费变化必须仍可 FAST 应用")
			}
		})
	}
}
