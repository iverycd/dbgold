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

// Recover already allocated, possibly empty INTERVAL partitions without solving
// the virtual expression or evaluating it on the source. A future real write
// and refresh prove that temporary constants did not replace the business rule.
func TestDamengNativeNonlinearIntervalIntegration(t *testing.T) {
	sd, ss := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dd, ds := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || sd == "" || ss == "" || dd == "" || ds == "" {
		t.Skip("需要两个预建空DM8 Schema、SOURCE/TARGET_DSN/SCHEMA及ALLOW_DDL/ALLOW_MV_DDL=1")
	}
	ctx, stop := context.WithTimeout(context.Background(), 6*time.Minute)
	defer stop()
	src, err := sql.Open("dm", sd)
	require.NoError(t, err)
	defer src.Close()
	dst, err := sql.Open("dm", dd)
	require.NoError(t, err)
	defer dst.Close()
	ss, err = damengnative.ResolveSchema(ctx, src, ss)
	require.NoError(t, err)
	ds, err = damengnative.ResolveSchema(ctx, dst, ds)
	require.NoError(t, err)
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, ss}, {dst, ds}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&count))
		require.Zero(t, count)
	}
	for _, tc := range []struct {
		name, columns, writeColumns, partition, old, removed, future string
		empty, uniqueKey                                             bool
	}{
		{"square", `"Id" INT NOT NULL,"Value" INT,"Key" INT GENERATED ALWAYS AS("Id"*"Id") VIRTUAL`, `"Id","Value"`, `INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`, "5,10", "9,99", "11,20", false, false},
		{"product", `"Id" INT NOT NULL,"Value" INT,"B" INT,"Key" INT GENERATED ALWAYS AS("Id"*"B") VIRTUAL`, `"Id","Value","B"`, `INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`, "2,10,7", "3,99,23", "5,20,17", false, false},
		{"case_abs_empty", `"Id" INT NOT NULL,"Value" INT,"Key" INT GENERATED ALWAYS AS(CASE WHEN "Id" IS NULL THEN 100 ELSE ABS("Id")*ABS("Id") END) VIRTUAL`, `"Id","Value"`, `INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`, "-5,10", "", "6,20", true, false},
		{"date_function", `"Id" INT NOT NULL,"Value" INT,"Key" DATE GENERATED ALWAYS AS(ADD_MONTHS(DATE '2020-01-01',"Id")) VIRTUAL`, `"Id","Value"`, `INTERVAL(NUMTOYMINTERVAL(1,'MONTH'))(PARTITION "Initial" VALUES LESS THAN(DATE '2020-01-01'))`, "4,10", "8,99", "11,20", false, false},
		{"checked_key_empty", `"Id" INT NOT NULL,"Value" INT,"Key" INT GENERATED ALWAYS AS("Id"*"Id") VIRTUAL NOT NULL,CONSTRAINT "GeneratedPositive" CHECK("Key">0) ENABLE NOVALIDATE`, `"Id","Value"`, `INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`, "5,10", "9,99", "11,20", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, mv := "NonlinearBase", "NonlinearSnapshot"
			sq := func(n string) string { return damengnative.Qualified(ss, n) }
			dq := func(n string) string { return damengnative.Qualified(ds, n) }
			exec := func(db *sql.DB, text string) { _, e := db.ExecContext(ctx, text); require.NoError(t, e, text) }
			var frozen *damengnative.Preflight
			t.Cleanup(func() {
				clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if frozen != nil {
					objects := damengnative.Order(frozen.Objects)
					for i := len(objects) - 1; i >= 0; i-- {
						if !objects[i].Managed && objects[i].DropSQL != "" {
							_, _ = dst.ExecContext(clean, objects[i].DropSQL)
						}
					}
				}
				_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq(mv))
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq(base)+" CASCADE CONSTRAINTS")
			})
			exec(src, "CREATE TABLE "+sq(base)+"("+tc.columns+`) PARTITION BY RANGE("Key") `+tc.partition)
			if tc.uniqueKey {
				exec(src, "CREATE UNIQUE INDEX "+sq("GeneratedKeyUnique")+" ON "+sq(base)+`("Key") GLOBAL`)
			}
			exec(src, "INSERT INTO "+sq(base)+"("+tc.writeColumns+") VALUES("+tc.old+")")
			if tc.removed != "" {
				exec(src, "INSERT INTO "+sq(base)+"("+tc.writeColumns+") VALUES("+tc.removed+")")
				exec(src, "DELETE FROM "+sq(base)+` WHERE "Value"=99`)
			}
			// Preserve exact names, allocated boundaries and allocation holes.
			var allocated string
			require.NoError(t, src.QueryRowContext(ctx, `SELECT PARTITION_NAME FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY PARTITION_POSITION DESC FETCH FIRST 1 ROWS ONLY`, ss, base).Scan(&allocated))
			exec(src, "ALTER TABLE "+sq(base)+" RENAME PARTITION "+damengnative.Quote(allocated)+` TO "HistoricalArchive"`)
			exec(src, "CREATE MATERIALIZED VIEW "+sq(mv)+` REFRESH FORCE ON DEMAND AS SELECT "Id","Value","Key" FROM `+sq(base)+` UNION ALL SELECT "Id","Value","Key" FROM `+sq(base))
			exec(src, "REFRESH MATERIALIZED VIEW "+sq(mv)+" COMPLETE")
			if tc.empty {
				exec(src, "DELETE FROM "+sq(base))
			} else {
				exec(src, "UPDATE "+sq(base)+` SET "Value"=11`)
			}
			partitions := func(db *sql.DB, schema string) [][]string {
				rows, e := db.QueryContext(ctx, `SELECT PARTITION_NAME,HIGH_VALUE,TABLESPACE_NAME FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=? ORDER BY PARTITION_POSITION`, schema, base)
				require.NoError(t, e)
				defer rows.Close()
				var out [][]string
				for rows.Next() {
					var name, high, space string
					require.NoError(t, rows.Scan(&name, &high, &space))
					out = append(out, []string{name, high, space})
				}
				require.NoError(t, rows.Err())
				return out
			}
			want := partitions(src, ss)
			r, e := source.NewDaMengNative(sd, ss, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := newNativeIntegrationWriter(t, dd, ds, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			var saved RepairManifest
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: ds, CaptureRepair: true, ValidateData: true, RepairSink: func(p RepairManifest) error { saved = p; return nil }})
			frozen = m.PreflightDamengNative(ctx)
			require.True(t, frozen.Ready, "%+v", frozen.Issues)
			report := m.Run(ctx)
			if report.HasFailures() {
				t.Logf("nonlinear verification=%+v content=%+v logs=%v", report.NativeSchema.Verification, report.DataValidation, drainLogs(m.job))
			}
			require.False(t, report.HasFailures())
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			require.Equal(t, want, partitions(dst, ds), "恢复不得新增NULL输入原表达式落入的其他分区")
			b, e := json.Marshal(saved)
			require.NoError(t, e)
			var reload RepairManifest
			require.NoError(t, json.Unmarshal(b, &reload))
			require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects))
			for _, side := range []struct {
				db *sql.DB
				q  func(string) string
			}{{src, sq}, {dst, dq}} {
				exec(side.db, "INSERT INTO "+side.q(base)+"("+tc.writeColumns+") VALUES("+tc.future+")")
			}
			var sourceKey, targetKey string
			require.NoError(t, src.QueryRowContext(ctx, `SELECT CAST("Key" AS VARCHAR(128)) FROM `+sq(base)+` WHERE "Value"=20`).Scan(&sourceKey))
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT CAST("Key" AS VARCHAR(128)) FROM `+dq(base)+` WHERE "Value"=20`).Scan(&targetKey))
			require.Equal(t, sourceKey, targetKey, "真实业务写入必须恢复原生成表达式")
			exec(dst, "REFRESH MATERIALIZED VIEW "+dq(mv)+" FORCE")
			var n int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(mv)+` WHERE "Value"=20`).Scan(&n))
			require.Equal(t, 2, n)
		})
	}
}
