package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type interruptedIntervalWriter struct {
	*target.DaMengNativeWriter
	once bool
}

func (w *interruptedIntervalWriter) RestoreDamengNativeIntervalPartitions(ctx context.Context, o damengnative.Object, u damengnative.Unit) error {
	observed := repair.WithObserver(ctx, func(event repair.Event) error {
		if event.Status == "succeeded" && w.once {
			w.once = false
			return context.Canceled
		}
		var err error
		if event.Error != "" {
			err = fmt.Errorf("%s", event.Error)
		}
		return repair.Observe(ctx, event.SQL, event.Status, err)
	})
	return w.DaMengNativeWriter.RestoreDamengNativeIntervalPartitions(observed, o, u)
}

// This tests the preparation unit's uncertain acknowledgement, before ordinary
// data copy starts. The full successful migration is covered by the other
// interval fixtures; an interrupted preparation must never claim task success.
func TestDamengNativeIntervalUnknownPreparationIntegration(t *testing.T) {
	sd, ss, dd, ds := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA"), os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || sd == "" || ss == "" || dd == "" || ds == "" {
		t.Skip("需要预建专用空DM8 Schema及DDL标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, e := sql.Open("dm", sd)
	require.NoError(t, e)
	defer src.Close()
	dst, e := sql.Open("dm", dd)
	require.NoError(t, e)
	defer dst.Close()
	ss, e = damengnative.ResolveSchema(ctx, src, ss)
	require.NoError(t, e)
	ds, e = damengnative.ResolveSchema(ctx, dst, ds)
	require.NoError(t, e)
	for _, side := range []struct {
		db *sql.DB
		s  string
	}{{src, ss}, {dst, ds}} {
		var n int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.s).Scan(&n))
		require.Zero(t, n, "需要初始为空的专用测试Schema")
	}
	for _, tc := range []struct{ attrsInterrupted, generated bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		attrsInterrupted := tc.attrsInterrupted
		t.Run(fmt.Sprint("interrupted_attributes_", attrsInterrupted, "_generated_", tc.generated), func(t *testing.T) {
			sq, dq := damengnative.Qualified(ss, "RecoverInterval"), damengnative.Qualified(ds, "RecoverInterval")
			exec := func(db *sql.DB, s string) { _, e := db.ExecContext(ctx, s); require.NoError(t, e, s) }
			var frozen *damengnative.Preflight
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				if frozen != nil {
					objects := damengnative.Order(frozen.Objects)
					for i := len(objects) - 1; i >= 0; i-- {
						if !objects[i].Managed && objects[i].DropSQL != "" {
							_, _ = dst.ExecContext(clean, objects[i].DropSQL)
						}
					}
				}
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq+" CASCADE CONSTRAINTS")
			}()
			generated, key := "", "A"
			if tc.generated {
				generated = `,"G" INT GENERATED ALWAYS AS("A"*2) VIRTUAL`
				key = "G"
			}
			exec(src, "CREATE TABLE "+sq+`("Id" BIGINT IDENTITY(10,3) PRIMARY KEY,"A" INT NOT NULL,"V" INT DEFAULT ON NULL 42 NOT NULL`+generated+`) PARTITION BY RANGE(`+damengnative.Quote(key)+`) INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(-5))`)
			exec(src, "INSERT INTO "+sq+`("A","V") VALUES(15,10)`)
			exec(src, "DELETE FROM "+sq)
			r, e := source.NewDaMengNative(sd, ss, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := target.NewDaMengNative(dd, ds, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			var saved RepairManifest
			m := NewMigrator(r, &interruptedIntervalWriter{DaMengNativeWriter: w, once: true}, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: ds, CaptureRepair: true, RepairSink: func(p RepairManifest) error { saved = p; return nil }})
			frozen = m.PreflightDamengNative(ctx)
			require.True(t, frozen.Ready, "%+v", frozen.Issues)
			report := m.Run(ctx)
			require.True(t, report.HasFailures(), "准备成功回执丢失不能误报任务完成")
			serialized, e := json.Marshal(saved)
			require.NoError(t, e)
			var reload RepairManifest
			require.NoError(t, json.Unmarshal(serialized, &reload))
			objectIndex := -1
			for i, o := range reload.Objects {
				if o.Native != nil && o.Native.Interval != nil {
					objectIndex = i
					break
				}
			}
			require.GreaterOrEqual(t, objectIndex, 0)
			object := reload.Objects[objectIndex]
			unitIndex := -1
			for i, u := range object.Native.Units {
				object.Units[i].Selected = u.Phase == "interval_prepare"
				if object.Units[i].Selected {
					unitIndex = i
					require.Equal(t, "unknown", object.Units[i].Status)
				}
			}
			require.GreaterOrEqual(t, unitIndex, 0)
			parts := func() []string {
				rows, e := dst.QueryContext(ctx, `SELECT p.PARTITION_NAME,p.HIGH_VALUE,o.ID FROM ALL_TAB_PARTITIONS p JOIN SYS.SYSOBJECTS b ON b.NAME=p.TABLE_NAME AND b.SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=p.TABLE_OWNER) JOIN SYS.SYSHPARTTABLEINFO h ON h.BASE_TABLE_ID=b.ID AND h.PARTITION_NAME=p.PARTITION_NAME JOIN SYS.SYSOBJECTS o ON o.ID=h.PART_TABLE_ID WHERE p.TABLE_OWNER=? AND p.TABLE_NAME='RecoverInterval' ORDER BY p.PARTITION_POSITION`, ds)
				require.NoError(t, e)
				defer rows.Close()
				var values []string
				for rows.Next() {
					var n, h string
					var id int64
					require.NoError(t, rows.Scan(&n, &h, &id))
					values = append(values, fmt.Sprint(n, "|", h, "|", id))
				}
				require.NoError(t, rows.Err())
				return values
			}
			before := parts()
			require.Len(t, before, 2)
			if attrsInterrupted {
				exec(dst, "ALTER TABLE "+dq+" DROP IDENTITY")
				exec(dst, "ALTER TABLE "+dq+` ALTER COLUMN "V" DROP DEFAULT`)
				exec(dst, "ALTER TABLE "+dq+` ALTER COLUMN "V" SET NULL`)
				var pk string
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT CONSTRAINT_NAME FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME='RecoverInterval' AND CONSTRAINT_TYPE='P'`, ds).Scan(&pk))
				exec(dst, "ALTER TABLE "+dq+" DISABLE CONSTRAINT "+damengnative.Quote(pk))
			}
			var events []repair.Event
			observed := repair.WithObserver(ctx, func(e repair.Event) error { events = append(events, e); return nil })
			units, e := target.ExecuteRepair(observed, w, object, object.Units, nil)
			require.NoError(t, e)
			require.Equal(t, "succeeded", units[unitIndex].Status)
			require.Equal(t, before, parts(), "不能反复创建或填平稀疏分区，也不能重建已有物理对象")
			if attrsInterrupted {
				require.NotEmpty(t, events, "需要受控恢复未清理属性")
			} else {
				require.Empty(t, events, "已完成且属性完整时必须纯读归并，不能重放typed恢复")
			}
			done, retry, e := w.ReconcileDamengNativeIntervalPartitions(ctx, *object.Native, object.Native.Units[unitIndex])
			require.NoError(t, e)
			require.True(t, done)
			require.False(t, retry)
			var n int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq).Scan(&n))
			require.Zero(t, n)
			e = dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq+" PARTITION FOR(0)").Scan(&n)
			require.Error(t, e)
			require.True(t, strings.Contains(e.Error(), "2629"), e)
			var current int64
			require.NoError(t, src.QueryRowContext(ctx, "SELECT IDENT_CURRENT(?) FROM DUAL", sq).Scan(&current))
			require.EqualValues(t, 10, current, "源预检不能消耗源identity")
			exec(dst, "INSERT INTO "+dq+`("A","V") VALUES(15,NULL)`)
			var id, v int64
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Id\",\"V\" FROM "+dq).Scan(&id, &v))
			require.EqualValues(t, 10, id, "该测试停在复制前，初始identity应原样还原")
			require.EqualValues(t, 42, v, "ON NULL默认值应已恢复")
		})
	}
}
