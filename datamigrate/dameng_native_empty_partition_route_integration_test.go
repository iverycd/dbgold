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
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

// A clustered key cannot be made nullable for an empty-table epoch. Its frozen
// temporary row must reach a real existing leaf; especially an INTERVAL probe
// must not leave a newly allocated partition behind.
func TestDamengNativeEmptyPartitionRouteHistoryIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("分区空基表验收需要预建空测试 Schema、SOURCE/TARGET_DSN/SCHEMA 及两个 ALLOW_DDL 标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
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
		db    *sql.DB
		owner string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var n int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.owner).Scan(&n))
		require.Zero(t, n, "必须使用初始为空的专用测试 Schema")
	}
	for _, tc := range []struct{ name, columns, key, partition, initial, future, writes, base string }{
		{"RANGE_negative", `"A" INT`, `"Id","A"`, `PARTITION BY RANGE("A")(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,-10`, `2,20,-9`, "", ""},
		{"RANGE_inclusive_minimum", `"A" INT`, `"Id","A"`, `PARTITION BY RANGE("A")(PARTITION "Only" VALUES EQU OR LESS THAN(-2147483648))`, `1,10,-2147483648`, `2,20,-2147483648`, "", ""},
		{"INTERVAL_negative", `"A" INT`, `"Id","A"`, `PARTITION BY RANGE("A") INTERVAL(10)(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,-10`, `2,20,-9`, "", ""},
		{"INTERVAL_existing_auto", `"A" INT`, `"Id","A"`, `PARTITION BY RANGE("A") INTERVAL(10)(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,15`, `2,20,0`, "", ""},
		{"INTERVAL_existing_inclusive_step_one", `"A" INT`, `"Id","A"`, `PARTITION BY RANGE("A") INTERVAL(1)(PARTITION "Only" VALUES EQU OR LESS THAN(-5))`, `1,10,0`, `2,20,-3`, "", ""},
		{"INTERVAL_existing_date_auto", `"A" DATE`, `"Id","A"`, `PARTITION BY RANGE("A") INTERVAL(NUMTOYMINTERVAL(1,'MONTH'))(PARTITION "Only" VALUES LESS THAN(TO_DATE('2020-01-01','YYYY-MM-DD')))`, `1,10,DATE '2020-05-15'`, `2,20,DATE '2020-02-15'`, "", ""},
		{"RANGE_date", `"A" DATE`, `"Id","A"`, `PARTITION BY RANGE("A")(PARTITION "Only" VALUES LESS THAN(TO_DATE('1990-01-01','YYYY-MM-DD')))`, `1,10,TO_DATE('1980-01-01','YYYY-MM-DD')`, `2,20,TO_DATE('1981-01-01','YYYY-MM-DD')`, "", ""},
		{"RANGE_LIST", `"A" INT,"B" INT`, `"Id","A","B"`, `PARTITION BY RANGE("A") SUBPARTITION BY LIST("B") SUBPARTITION TEMPLATE(SUBPARTITION "B5" VALUES(5),SUBPARTITION "B7" VALUES(7))(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,-10,5`, `2,20,-9,7`, "", ""},
		{"RANGE_generated_key", `"A" INT GENERATED ALWAYS AS("Id"*2) VIRTUAL`, "", `PARTITION BY RANGE("A")(PARTITION "Only" VALUES LESS THAN(-5))`, `-5,10`, `-6,20`, "", ""},
		{"LIST_LIST_LIST", `"A" INT,"B" INT,"C" INT`, `"Id","A","B","C"`, `PARTITION BY LIST("A") SUBPARTITION BY LIST("B") SUBPARTITION TEMPLATE(SUBPARTITION "B7" VALUES(7)), SUBPARTITION BY LIST("C") SUBPARTITION TEMPLATE(SUBPARTITION "C9" VALUES(9))(PARTITION "A5" VALUES(5))`, `1,10,5,7,9`, `2,20,5,7,9`, "", ""},
		{"INTERVAL_existing_generated_double", `"A" INT GENERATED ALWAYS AS("Id"*2) VIRTUAL`, "", `PARTITION BY RANGE("A") INTERVAL(10)(PARTITION "Only" VALUES LESS THAN(-5))`, `42,10`, `0,20`, "", ""},
		{"INTERVAL_existing_generated_multi", `"A" INT,"B" INT,"G" INT GENERATED ALWAYS AS("A"+"B"*2) VIRTUAL`, "", `PARTITION BY RANGE("G") INTERVAL(10)(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,1,40`, `2,20,0,0`, `("Id","Value","A","B")`, ""},
		{"INTERVAL_existing_generated_lattice", `"B" INT,"G" INT GENERATED ALWAYS AS("Id"*2+"B"*3) VIRTUAL`, "", `PARTITION BY RANGE("G") INTERVAL(1)(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,1`, `2,20,0`, `("Id","Value","B")`, ""},
		{"INTERVAL_existing_generated_decimal", `"A" DECIMAL(10,2),"G" DECIMAL(12,2) GENERATED ALWAYS AS("A"*2+0.5) VIRTUAL`, "", `PARTITION BY RANGE("G") INTERVAL(1)(PARTITION "Only" VALUES LESS THAN(-5))`, `1,10,42.12`, `2,20,0.1`, `("Id","Value","A")`, ""},
		{"INTERVAL_existing_generated_negative", `"G" INT GENERATED ALWAYS AS(10-"Id"*3) VIRTUAL`, "", `PARTITION BY RANGE("G") INTERVAL(10)(PARTITION "Only" VALUES LESS THAN(-5))`, `-25,10`, `0,20`, "", ""},
		{"INTERVAL_existing_generated_constant_string", `"G" INT GENERATED ALWAYS AS(10) VIRTUAL`, "", `PARTITION BY RANGE("G") INTERVAL(10)(PARTITION "Only" VALUES LESS THAN(-5))`, `'old',10`, `'new',20`, "", `"Id" VARCHAR(40) NOT NULL,"Value" VARCHAR(40)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sq := func(n string) string { return damengnative.Qualified(srcSchema, n) }
			dq := func(n string) string { return damengnative.Qualified(dstSchema, n) }
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
				_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq("RouteSnapshot"))
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq("RouteBase")+" CASCADE CONSTRAINTS")
			}()
			key := ""
			if tc.key != "" {
				key = `,CLUSTER PRIMARY KEY(` + tc.key + `)`
			}
			base := `"Id" INT,"Value" INT`
			if tc.base != "" {
				base = tc.base
			}
			exec(src, "CREATE TABLE "+sq("RouteBase")+`(`+base+`,`+tc.columns+key+`) `+tc.partition)
			insertColumns := ""
			if tc.key == "" {
				insertColumns = `("Id","Value")`
			}
			if tc.writes != "" {
				insertColumns = tc.writes
			}
			exec(src, "INSERT INTO "+sq("RouteBase")+insertColumns+" VALUES("+tc.initial+")")
			exec(src, "CREATE MATERIALIZED VIEW "+sq("RouteSnapshot")+` REFRESH FORCE ON DEMAND AS SELECT "Id","Value" FROM `+sq("RouteBase")+` UNION ALL SELECT "Id","Value" FROM `+sq("RouteBase"))
			exec(src, "REFRESH MATERIALIZED VIEW "+sq("RouteSnapshot")+" COMPLETE")
			exec(src, "DELETE FROM "+sq("RouteBase"))
			var initialPartitions int
			require.NoError(t, src.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME='RouteBase'`, srcSchema).Scan(&initialPartitions))
			r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			var manifest RepairManifest
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, PageSize: 1, RepairSink: func(p RepairManifest) error { manifest = p; return nil }})
			frozen = m.PreflightDamengNative(ctx)
			require.True(t, frozen.Ready, "%+v", frozen.Issues)
			var n int
			require.NoError(t, src.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME='RouteBase'`, srcSchema).Scan(&n))
			require.Equal(t, initialPartitions, n, "只读预检不能创建 INTERVAL 分区")
			report := m.Run(ctx)
			if report.HasFailures() {
				b, _ := json.Marshal(report)
				t.Logf("partition route report: %s; logs=%v", b, drainLogs(m.job))
			}
			require.False(t, report.HasFailures())
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			b, e := json.Marshal(manifest)
			require.NoError(t, e)
			var reload RepairManifest
			require.NoError(t, json.Unmarshal(b, &reload))
			require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects))
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("RouteBase")).Scan(&n))
			require.Zero(t, n, "临时行必须删除")
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME='RouteBase'`, dstSchema).Scan(&n))
			require.Equal(t, initialPartitions, n, "恢复状态不能创建新 INTERVAL 分区")
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("RouteSnapshot")+` WHERE "Value"=10`).Scan(&n))
			require.Equal(t, 2, n, "保留旧快照及重复值")
			if tc.name == "INTERVAL_existing_auto" || tc.name == "INTERVAL_existing_date_auto" {
				missing := "0"
				if tc.name == "INTERVAL_existing_date_auto" {
					missing = "DATE '2020-02-15'"
				}
				var ignored int
				e = dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("RouteBase")+" PARTITION FOR("+missing+")").Scan(&ignored)
				require.Error(t, e, "恢复已有auto分区不能填平源稀疏间隔的洞")
			}
			exec(dst, "INSERT INTO "+dq("RouteBase")+insertColumns+" VALUES("+tc.future+")")
			exec(dst, "REFRESH MATERIALIZED VIEW "+dq("RouteSnapshot")+" FORCE")
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("RouteSnapshot")+` WHERE "Value"=20`).Scan(&n))
			require.Equal(t, 2, n, "后续 FORCE 必须读取新的基表内容")
		})
	}
}
