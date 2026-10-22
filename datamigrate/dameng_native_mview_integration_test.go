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
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

// Full pipeline acceptance uses two explicitly named precreated empty schemas.
// It covers catalog extraction, stored snapshot copy, the persisted baseline,
// full ordinary-value validation, exact policy restoration and future FAST.
func TestDamengNativeMaterializedPipelineIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("完整物化迁移实库验收需独立空源目标 Schema、DAMENG_NATIVE_ALLOW_DDL=1、DAMENG_NATIVE_ALLOW_MV_DDL=1 及 SOURCE/TARGET_DSN/SCHEMA")
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
	sp, err := damengnative.ReadProfile(ctx, src, srcSchema)
	require.NoError(t, err)
	dp, err := damengnative.ReadProfile(ctx, dst, dstSchema)
	require.NoError(t, err)
	require.False(t, nativeSameDatabase(sp, dp) && srcSchema == dstSchema)
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON o.SCHID=s.ID WHERE s.NAME=? AND s.TYPE$='SCH'`, side.schema).Scan(&count))
		require.Zero(t, count, "专用测试模式必须最初为空")
	}
	for n, variant := range []struct{ method, rule string }{{"FAST", "PRIMARY KEY"}, {"FORCE", "PRIMARY KEY"}, {"FAST", "ROWID"}, {"FORCE", "ROWID"}} {
		t.Run(variant.method+"_"+variant.rule, func(t *testing.T) {
			prefix := fmt.Sprintf("DmMP%X%d", time.Now().UnixNano(), n)
			base, mv, index := prefix+"B", prefix+"M", prefix+"I"
			sq := func(name string) string { return damengnative.Qualified(srcSchema, name) }
			dq := func(name string) string { return damengnative.Qualified(dstSchema, name) }
			helper := "DBGOLD_MV_" + damengnative.Fingerprint([]string{srcSchema, mv})[:20]
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				for _, side := range []struct {
					db *sql.DB
					q  func(string) string
				}{{dst, dq}, {src, sq}} {
					for _, ddl := range []string{"DROP MATERIALIZED VIEW " + side.q(mv), "DROP TABLE " + side.q(helper) + " CASCADE CONSTRAINTS", "DROP TABLE " + side.q(base) + " CASCADE CONSTRAINTS"} {
						_, _ = side.db.ExecContext(clean, ddl)
					}
				}
			}()
			exec := func(db *sql.DB, statement string) {
				_, e := db.ExecContext(ctx, statement)
				require.NoError(t, e, statement)
			}
			exec(src, "CREATE TABLE "+sq(base)+` ("Id" INT PRIMARY KEY,"Value" INT)`)
			exec(src, "INSERT INTO "+sq(base)+` VALUES(-1,-1)`)
			exec(src, "DELETE FROM "+sq(base)+` WHERE "Id"=-1`)
			exec(src, "INSERT INTO "+sq(base)+` VALUES(1,10)`)
			exec(src, "INSERT INTO "+sq(base)+` VALUES(2,10)`)
			exec(src, "CREATE MATERIALIZED VIEW LOG ON "+sq(base)+" WITH "+variant.rule)
			projection := `"Id","Value"`
			if variant.rule == "ROWID" {
				projection += `,ROWID AS "Rid"`
			}
			exec(src, "CREATE MATERIALIZED VIEW "+sq(mv)+" REFRESH "+variant.method+" WITH "+variant.rule+" ON DEMAND AS SELECT "+projection+" FROM "+sq(base))
			exec(src, "CREATE INDEX "+sq(index)+" ON "+sq(mv)+` ("Value")`)
			r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
			require.NoError(t, e)
			defer r.Close()
			w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
			require.NoError(t, e)
			defer w.Close()
			job := &Job{LogCh: make(chan string, 10000)}
			m := NewMigrator(r, w, job, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, PageSize: 1, MaxParallel: 1})
			preview := m.PreflightDamengNative(ctx)
			require.True(t, preview.Ready, "%+v", preview.Issues)
			report := m.Run(ctx)
			if report.HasFailures() {
				encoded, _ := json.Marshal(report)
				t.Logf("native MV report: %s", encoded)
				t.Logf("native MV logs: %v", drainLogs(job))
			}
			require.False(t, report.HasFailures())
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			if variant.rule == "ROWID" {
				noted := false
				for _, table := range report.DataValidation.Tables {
					if strings.Contains(table.Note, "ROWID") {
						noted = true
					}
				}
				require.True(t, noted, "ROWID 物理映射必须明确显示语义校验范围")
			}
			var indexes int
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_INDEXES WHERE OWNER=? AND INDEX_NAME=?`, dstSchema, index).Scan(&indexes))
			require.Equal(t, 1, indexes, "用户命名物化视图索引必须保留")
			var ddl string
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(?,?,?)`, "MATERIALIZED_VIEW", mv, dstSchema).Scan(&ddl))
			policy, e := damengnative.MaterializedPolicy(ddl)
			require.NoError(t, e)
			require.Contains(t, policy, "REFRESH "+variant.method)
			require.Contains(t, policy, "WITH "+variant.rule)
			exec(dst, "UPDATE "+dq(base)+` SET "Value"=99 WHERE "Id"=1`)
			exec(dst, "INSERT INTO "+dq(base)+` VALUES(3,30)`)
			exec(dst, "REFRESH MATERIALIZED VIEW "+dq(mv)+" FAST")
			var value, count int
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT "Value" FROM `+dq(mv)+` WHERE "Id"=1`).Scan(&value))
			require.Equal(t, 99, value)
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(mv)).Scan(&count))
			require.Equal(t, 3, count)
			if variant.rule == "ROWID" {
				require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+dq(mv)+` M JOIN `+dq(base)+` B ON M."Id"=B."Id" WHERE M."Rid"<>B.ROWID`).Scan(&count))
				require.Zero(t, count)
			}
		})
	}
}
