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

// FORCE is legal without any materialized log, including queries for which
// FAST is unavailable. Preserve the stored UNION ALL result and duplicates,
// then prove the restored FORCE policy still rebuilds from the current tables.
func TestDamengNativeForceWithoutLogUnionIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("无日志 FORCE 验收需要预建空测试 Schema、SOURCE/TARGET_DSN/SCHEMA 及两个 ALLOW_DDL 标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.owner).Scan(&count))
		require.Zero(t, count, "必须使用初始为空的专用测试 Schema")
	}
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
		_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq("UnionSnapshot"))
		_, _ = src.ExecContext(clean, "DROP TABLE "+sq("NoLogA")+" CASCADE CONSTRAINTS")
		_, _ = src.ExecContext(clean, "DROP TABLE "+sq("NoLogB")+" CASCADE CONSTRAINTS")
	}()
	for _, name := range []string{"NoLogA", "NoLogB"} {
		exec(src, "CREATE TABLE "+sq(name)+` ("Id" INT,"Value" INT)`)
	}
	exec(src, "INSERT INTO "+sq("NoLogA")+" VALUES(1,10)")
	exec(src, "INSERT INTO "+sq("NoLogA")+" VALUES(1,10)")
	exec(src, "INSERT INTO "+sq("NoLogA")+" VALUES(2,20)")
	exec(src, "INSERT INTO "+sq("NoLogB")+" VALUES(7,70)")
	exec(src, "CREATE MATERIALIZED VIEW "+sq("UnionSnapshot")+` REFRESH FORCE ON DEMAND AS SELECT "Id","Value" FROM `+sq("NoLogA")+` UNION ALL SELECT "Id","Value" FROM `+sq("NoLogB"))
	exec(src, "REFRESH MATERIALIZED VIEW "+sq("UnionSnapshot")+" COMPLETE")
	exec(src, "UPDATE "+sq("NoLogA")+` SET "Value"=11 WHERE "Id"=1`)
	exec(src, "DELETE FROM "+sq("NoLogA")+` WHERE "Id"=2`)
	exec(src, "DELETE FROM "+sq("NoLogB"))
	exec(src, "INSERT INTO "+sq("NoLogB")+" VALUES(8,80)")
	r, err := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var manifest RepairManifest
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, PageSize: 1, MaxParallel: 2, RepairSink: func(p RepairManifest) error { manifest = p; return nil }})
	frozen = m.PreflightDamengNative(ctx)
	require.True(t, frozen.Ready, "%+v", frozen.Issues)
	found := false
	for _, o := range frozen.Objects {
		if o.Materialized != nil && o.Materialized.History != nil {
			found = true
			require.Empty(t, o.Materialized.History.Logs)
		}
	}
	require.True(t, found)
	report := m.Run(ctx)
	if report.HasFailures() {
		b, _ := json.Marshal(report)
		t.Logf("FORCE no-log report: %s; logs=%v", b, drainLogs(m.job))
	}
	require.False(t, report.HasFailures())
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	b, err := json.Marshal(manifest)
	require.NoError(t, err)
	var reload RepairManifest
	require.NoError(t, json.Unmarshal(b, &reload))
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects))
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("UnionSnapshot")+` WHERE "Id"=1 AND "Value"=10`).Scan(&count))
	require.Equal(t, 2, count, "原快照的重复行必须保留")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("UnionSnapshot")+` WHERE "Id" IN (2,7)`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("UnionSnapshot")+` WHERE "Id"=8`).Scan(&count))
	require.Zero(t, count)
	exec(dst, "UPDATE "+dq("NoLogA")+` SET "Value"=13 WHERE "Id"=1`)
	exec(dst, "INSERT INTO "+dq("NoLogB")+" VALUES(9,90)")
	exec(dst, "REFRESH MATERIALIZED VIEW "+dq("UnionSnapshot")+" FORCE")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("UnionSnapshot")).Scan(&count))
	require.Equal(t, 4, count)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("UnionSnapshot")+` WHERE "Id"=1 AND "Value"=13`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("UnionSnapshot")+` WHERE "Id" IN (2,7)`).Scan(&count))
	require.Zero(t, count)
}
