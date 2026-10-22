package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type interruptedHistoryWriter struct {
	*target.DaMengNativeWriter
	once bool
}

func (w *interruptedHistoryWriter) RestoreDamengNativeMaterializedHistory(ctx context.Context, object damengnative.Object, unit damengnative.Unit) error {
	observed := repair.WithObserver(ctx, func(event repair.Event) error {
		if event.Status == "succeeded" && w.once {
			w.once = false
			// Model a lost acknowledgement after every server exchange completed.
			return context.Canceled
		}
		return repair.Observe(ctx, event.SQL, event.Status, func() error {
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return nil
		}())
	})
	return w.DaMengNativeWriter.RestoreDamengNativeMaterializedHistory(observed, object, unit)
}

// This fixture deliberately freezes two different historical snapshots over
// one log. Both must survive migration until an explicit real FAST refresh;
// a successful CREATE or a refresh of the current query is insufficient.
func TestDamengNativeStaleMaterializedHistoryIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("陈旧快照验收需要专用预建空 Schema、SOURCE/TARGET_DSN/SCHEMA 及 DAMENG_NATIVE_ALLOW_DDL=1、DAMENG_NATIVE_ALLOW_MV_DDL=1")
	}
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Minute)
	defer stop()
	src, err := sql.Open("dm", srcDSN)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	defer dst.Close()
	srcSchema, err = damengnative.ResolveSchema(ctx, src, srcSchema)
	require.NoError(t, err)
	dstSchema, err = damengnative.ResolveSchema(ctx, dst, dstSchema)
	require.NoError(t, err)
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&count))
		require.Zero(t, count, "必须使用初始为空的专用测试 Schema")
	}
	for _, variant := range []struct {
		method, rule string
		resume       bool
		partitioning string
		emptyBase    bool
		noLog        bool
		unchanged    bool
		needsFull    bool
	}{
		{method: "FAST", rule: "PRIMARY KEY"},
		{method: "FORCE", rule: "PRIMARY KEY"},
		{method: "FAST", rule: "ROWID"},
		{method: "FORCE", rule: "ROWID"},
		{method: "FAST", rule: "ROWID", resume: true},
		{method: "FAST", rule: "PRIMARY KEY", partitioning: "RANGE"},
		{method: "FAST", rule: "PRIMARY KEY", partitioning: "HASH"},
		{method: "FORCE", rule: "PRIMARY KEY", resume: true, partitioning: "HASH"},
		{method: "FAST", rule: "ROWID", partitioning: "HASH"},
		{method: "FAST", rule: "PRIMARY KEY", emptyBase: true},
		{method: "FORCE", rule: "ROWID", emptyBase: true},
		{method: "FORCE", rule: "PRIMARY KEY", noLog: true},
		{method: "FORCE", rule: "ROWID", noLog: true},
		{method: "FAST", rule: "PRIMARY KEY", unchanged: true},
		{method: "FORCE", rule: "PRIMARY KEY", needsFull: true},
	} {
		label := variant.method + "_" + variant.rule
		if variant.resume {
			label += "_RESUME"
		}
		if variant.partitioning != "" {
			label += "_" + variant.partitioning
		}
		if variant.emptyBase {
			label += "_EMPTY"
		}
		if variant.noLog {
			label += "_NOLOG"
		}
		if variant.unchanged {
			label += "_UNCHANGED"
		}
		if variant.needsFull {
			label += "_NEEDS_FULL"
		}
		t.Run(label, func(t *testing.T) {
			names := []string{"HistoryBase", "OlderSnapshot", "NewerSnapshot"}
			sq := func(n string) string { return damengnative.Qualified(srcSchema, n) }
			dq := func(n string) string { return damengnative.Qualified(dstSchema, n) }
			exec := func(db *sql.DB, statement string) {
				_, e := db.ExecContext(ctx, statement)
				require.NoError(t, e, statement)
			}
			waitNativeEpoch := func(db *sql.DB, schema, name string) {
				// This DM8 build also loses pending logs in an entirely native
				// two-view fixture created/refreshed at the same clock boundary.
				// Keep the source fixture valid independently of the importer.
				var cutoff string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME=?`, schema, name).Scan(&cutoff))
				last, e := time.Parse("2006-01-02 15:04:05.000000", cutoff)
				require.NoError(t, e)
				threshold := last.Truncate(time.Second).Add(time.Second + 110*time.Millisecond).Format("2006-01-02 15:04:05.000000")
				require.Eventually(t, func() bool {
					var ready int
					e := db.QueryRowContext(ctx, `SELECT CASE WHEN NOW(6)>=TO_TIMESTAMP(?,'YYYY-MM-DD HH24:MI:SS.FF6') THEN 1 ELSE 0 END FROM DUAL`, threshold).Scan(&ready)
					require.NoError(t, e)
					return ready == 1
				}, 5*time.Second, 20*time.Millisecond, "等待服务端进入原生快照水位之后的稳定时间段")
			}
			var frozen *damengnative.Preflight
			defer func() {
				clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if frozen != nil {
					ordered := damengnative.Order(frozen.Objects)
					for i := len(ordered) - 1; i >= 0; i-- {
						if !ordered[i].Managed && ordered[i].DropSQL != "" {
							_, _ = dst.ExecContext(clean, ordered[i].DropSQL)
						}
					}
				}
				for _, mv := range names[1:] {
					_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq(mv))
					if variant.partitioning != "" {
						_, _ = src.ExecContext(clean, "DROP TABLE "+sq(mv+"Storage")+" CASCADE CONSTRAINTS")
					}
				}
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq(names[0])+" CASCADE CONSTRAINTS")
			}()
			key := " PRIMARY KEY"
			if variant.rule == "ROWID" {
				key = ""
			}
			valueNullability := ""
			if variant.emptyBase {
				valueNullability = " NOT NULL"
			}
			exec(src, "CREATE TABLE "+sq(names[0])+` ("Id" INT`+key+`,"Value" INT`+valueNullability+`)`)
			// Different physical allocation histories prevent accidental ROWID equality.
			exec(src, "INSERT INTO "+sq(names[0])+" VALUES(-1,-1)")
			exec(src, "DELETE FROM "+sq(names[0])+` WHERE "Id"=-1`)
			if variant.emptyBase {
				// Generated CHECKs can share the same referenced column. Their
				// original expressions/states must survive empty-table touching.
				exec(src, "ALTER TABLE "+sq(names[0])+` ADD CHECK("Value">0)`)
				exec(src, "ALTER TABLE "+sq(names[0])+` ADD CHECK("Value"<100)`)
			}
			exec(src, "INSERT INTO "+sq(names[0])+" VALUES(1,10)")
			exec(src, "INSERT INTO "+sq(names[0])+" VALUES(2,20)")
			if variant.rule == "ROWID" {
				exec(src, "INSERT INTO "+sq(names[0])+" VALUES(1,10)")
			}
			if !variant.noLog {
				exec(src, "CREATE MATERIALIZED VIEW LOG ON "+sq(names[0])+" WITH "+variant.rule)
			}
			projection := `"Id","Value"`
			if variant.rule == "ROWID" {
				projection += `,ROWID AS "Rid"`
			}
			create := func(name string) {
				prebuilt := ""
				if variant.partitioning != "" {
					partitioning := ` PARTITION BY RANGE("Id") (PARTITION "Low" VALUES LESS THAN(2),PARTITION "High" VALUES LESS THAN(MAXVALUE))`
					if variant.partitioning == "HASH" {
						partitioning = ` PARTITION BY HASH("Id") PARTITIONS 2`
					}
					columns := ` ("Id" INT,"Value" INT)`
					if variant.rule == "ROWID" {
						columns = ` ("Id" INT,"Value" INT,"Rid" ROWID)`
					}
					exec(src, "CREATE TABLE "+sq(name+"Storage")+columns+partitioning)
					exec(src, "INSERT INTO "+sq(name+"Storage")+" SELECT "+projection+" FROM "+sq(names[0]))
					prebuilt = " FOR " + sq(name+"Storage") + " ON PREBUILT TABLE"
				}
				exec(src, "CREATE MATERIALIZED VIEW "+sq(name)+prebuilt+" REFRESH "+variant.method+" WITH "+variant.rule+" ON DEMAND AS SELECT "+projection+" FROM "+sq(names[0]))
				if variant.partitioning != "" {
					exec(src, "REFRESH MATERIALIZED VIEW "+sq(name)+" COMPLETE")
				}
				waitNativeEpoch(src, srcSchema, name)
			}
			create(names[1])
			if variant.unchanged {
				exec(src, "UPDATE "+sq(names[0])+` SET "Value"="Value" WHERE "Id"=1`)
			} else {
				exec(src, "UPDATE "+sq(names[0])+` SET "Value"=11 WHERE "Id"=1`)
				exec(src, "DELETE FROM "+sq(names[0])+` WHERE "Id"=2`)
				exec(src, "INSERT INTO "+sq(names[0])+" VALUES(3,30)")
			}
			create(names[2])
			if variant.unchanged {
				exec(src, "UPDATE "+sq(names[0])+` SET "Value"="Value" WHERE "Id"=1`)
			} else {
				exec(src, "UPDATE "+sq(names[0])+` SET "Value"=12 WHERE "Id"=1`)
				exec(src, "DELETE FROM "+sq(names[0])+` WHERE "Id"=3`)
				exec(src, "INSERT INTO "+sq(names[0])+" VALUES(4,40)")
			}
			if variant.emptyBase {
				exec(src, "DELETE FROM "+sq(names[0]))
			}
			if variant.needsFull {
				exec(src, "ALTER MATERIALIZED VIEW "+sq(names[1])+" NEVER REFRESH")
				exec(src, "ALTER MATERIALIZED VIEW "+sq(names[1])+" REFRESH FORCE ON DEMAND")
			}
			r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			var saved RepairManifest
			var runWriter target.Writer = w
			if variant.resume {
				runWriter = &interruptedHistoryWriter{DaMengNativeWriter: w, once: true}
			}
			m := NewMigrator(r, runWriter, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, PageSize: 1, MaxParallel: 3, RepairSink: func(manifest RepairManifest) error { saved = manifest; return nil }})
			frozen = m.PreflightDamengNative(ctx)
			require.True(t, frozen.Ready, "%+v", frozen.Issues)
			require.True(t, nativeHasMaterializedHistory(frozen.Objects))
			report := m.Run(ctx)
			if variant.resume {
				require.True(t, report.HasFailures(), "丢失组成功响应不能被视为迁移成功")
				resumed := false
				for i := range saved.Objects {
					object := saved.Objects[i]
					if object.Native == nil || object.Native.Materialized == nil || object.Native.Materialized.History == nil {
						continue
					}
					for j := range object.Units {
						object.Units[j].Selected = object.Native.Units[j].Phase == "mv_history"
						if object.Units[j].Selected {
							require.Equal(t, "unknown", object.Units[j].Status)
						}
					}
					units, e := target.ExecuteRepair(ctx, w, object, object.Units, nil)
					require.NoError(t, e, "必须先核对目录，再从不可变输入重建，不能反向重放EXCHANGE")
					saved.Objects[i].Units = units
					resumed = true
				}
				require.True(t, resumed)
				proof := damengnative.WithFinalizationVerifier(ctx, func(proof context.Context) error {
					return VerifyNativeRepairFinalization(proof, r, w, m.cfg, saved.Objects)
				})
				for i := range saved.Objects {
					object := saved.Objects[i]
					if object.Native == nil || object.Native.Managed {
						continue
					}
					selected := false
					for j := range object.Units {
						object.Units[j].Selected = object.Native.Units[j].Phase == "mv_restore" && object.Units[j].Status != "succeeded"
						selected = selected || object.Units[j].Selected
					}
					if !selected {
						continue
					}
					units, e := target.ExecuteRepair(proof, w, object, object.Units, nil)
					require.NoError(t, e)
					saved.Objects[i].Units = units
				}
			} else if report.HasFailures() {
				b, _ := json.Marshal(report)
				t.Logf("history report: %s; logs=%v", b, drainLogs(m.job))
			}
			if !variant.resume {
				require.False(t, report.HasFailures())
				require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			}
			serialized, e := json.Marshal(saved)
			require.NoError(t, e)
			var reload RepairManifest
			require.NoError(t, json.Unmarshal(serialized, &reload))
			for _, object := range reload.Objects {
				if object.Native != nil && !object.Native.Managed {
					require.True(t, object.TrustedNative(), object.Name)
				}
			}
			require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects), "重载的完整冻结历史组必须可通过恢复路径的源目录、目标定义及内容核验")
			for i, name := range names[1:] {
				var value, absent int
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT MIN("Value") FROM `+dq(name)+` WHERE "Id"=1`).Scan(&value))
				expected := 10 + i
				if variant.unchanged {
					expected = 10
				}
				require.Equal(t, expected, value, "必须保留各自陈旧快照，不能用当前查询替代")
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+dq(name)+` WHERE "Id"=4`).Scan(&absent))
				require.Zero(t, absent)
				if variant.partitioning != "" {
					var partitions int
					require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=?`, dstSchema, name+"Storage").Scan(&partitions))
					require.Equal(t, 2, partitions, "原预建承载表的分区必须保留")
				}
			}
			// A new target change must be ordered after the imported pending logs.
			exec(dst, "UPDATE "+dq(names[0])+` SET "Value"=13 WHERE "Id"=1`)
			exec(dst, "INSERT INTO "+dq(names[0])+" VALUES(5,50)")
			for _, name := range names[1:] {
				waitNativeEpoch(dst, dstSchema, names[2])
				method := "FAST"
				if variant.noLog || variant.needsFull && name == names[1] {
					method = "FORCE"
				}
				exec(dst, "REFRESH MATERIALIZED VIEW "+dq(name)+" "+method)
				var wrong int
				expected := 3
				if variant.rule == "ROWID" {
					expected = 4
				}
				if variant.emptyBase {
					expected = 1
				}
				require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(name)).Scan(&wrong))
				if wrong != expected {
					for _, query := range []string{`SELECT "Id","Value" FROM ` + dq(name), `SELECT * FROM ` + dq("MLOG$_"+names[0]), `SELECT MVIEW_NAME,STALENESS,TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME='` + dstSchema + `')`} {
						rows, e := dst.QueryContext(ctx, query)
						if e != nil {
							t.Log(e)
							continue
						}
						cols, _ := rows.Columns()
						for rows.Next() {
							values := make([]any, len(cols))
							ptrs := make([]any, len(cols))
							for i := range values {
								ptrs[i] = &values[i]
							}
							_ = rows.Scan(ptrs...)
							t.Logf("history diagnostic %v = %v", cols, values)
						}
						_ = rows.Close()
					}
				}
				require.Equal(t, expected, wrong)
				absentIDs := "2,3"
				if variant.unchanged {
					absentIDs = "3,4"
				}
				for _, query := range []string{`SELECT COUNT(*) FROM ` + dq(name) + ` WHERE "Id" IN (` + absentIDs + `)`, `SELECT COUNT(*) FROM ` + dq(name) + ` WHERE "Id"=1 AND "Value"<>13`} {
					require.NoError(t, dst.QueryRowContext(ctx, query).Scan(&wrong))
					require.Zero(t, wrong, fmt.Sprintf("历史日志与新日志必须同时被 FAST 消费: %s", name))
				}
				if variant.rule == "ROWID" {
					require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(name)+` M WHERE NOT EXISTS(SELECT 1 FROM `+dq(names[0])+` B WHERE B.ROWID=M."Rid" AND B."Id"=M."Id" AND B."Value"=M."Value")`).Scan(&wrong))
					require.Zero(t, wrong)
				}
			}
		})
	}
}
