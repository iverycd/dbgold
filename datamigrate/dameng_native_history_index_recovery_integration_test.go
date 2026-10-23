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

type failedHistoryIndexWriter struct {
	*target.DaMengNativeWriter
	index string
}

func (w *failedHistoryIndexWriter) ExecuteDamengNative(ctx context.Context, query string) error {
	if strings.Contains(query, damengnative.Quote(w.index)) && strings.HasPrefix(strings.ToUpper(query), "CREATE ") {
		if err := repair.Observe(ctx, query, "running", nil); err != nil {
			return err
		}
		err := fmt.Errorf("injected deferred index installation failure")
		if saved := repair.Observe(ctx, query, "failed", err); saved != nil {
			return saved
		}
		return err
	}
	return w.DaMengNativeWriter.ExecuteDamengNative(ctx, query)
}

// Success of the irreversible history group must survive a later index error.
// A reloaded ledger repairs only that index; the snapshot and native cutoff
// remain exact until the caller explicitly requests a future FAST refresh.
func TestDamengNativeHistoryIndexRecoveryIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("历史恢复后索引补迁验收需要专用预建空 Schema、SOURCE/TARGET_DSN/SCHEMA 及 DDL、MV_DDL 开关")
	}
	ctx, stop := context.WithTimeout(context.Background(), 4*time.Minute)
	defer stop()
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
	sq := func(n string) string { return damengnative.Qualified(srcSchema, n) }
	dq := func(n string) string { return damengnative.Qualified(dstSchema, n) }
	exec := func(db *sql.DB, q string) { _, e := db.ExecContext(ctx, q); require.NoError(t, e, q) }
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
		for _, q := range []string{"DROP MATERIALIZED VIEW " + sq("Snapshot"), "DROP TABLE " + sq("Storage") + " CASCADE CONSTRAINTS", "DROP TABLE " + sq("Base") + " CASCADE CONSTRAINTS"} {
			_, _ = src.ExecContext(clean, q)
		}
	}()
	exec(src, "CREATE TABLE "+sq("Base")+` ("Id" INT PRIMARY KEY,"Value" INT)`)
	exec(src, "INSERT INTO "+sq("Base")+" VALUES(1,10)")
	exec(src, "CREATE MATERIALIZED VIEW LOG ON "+sq("Base")+" WITH PRIMARY KEY")
	exec(src, "CREATE TABLE "+sq("Storage")+` ("Id" INT,"Value" INT) PARTITION BY RANGE("Id") (PARTITION "Low" VALUES LESS THAN(2),PARTITION "High" VALUES LESS THAN(MAXVALUE))`)
	exec(src, "INSERT INTO "+sq("Storage")+" SELECT * FROM "+sq("Base"))
	exec(src, "CREATE MATERIALIZED VIEW "+sq("Snapshot")+" FOR "+sq("Storage")+" ON PREBUILT TABLE REFRESH FAST WITH PRIMARY KEY ON DEMAND AS SELECT \"Id\",\"Value\" FROM "+sq("Base"))
	exec(src, "REFRESH MATERIALIZED VIEW "+sq("Snapshot")+" COMPLETE")
	time.Sleep(1200 * time.Millisecond)
	exec(src, "UPDATE "+sq("Base")+` SET "Value"=11 WHERE "Id"=1`)
	exec(src, "CREATE INDEX "+sq("GlobalIndex")+" ON "+sq("Storage")+`("Value") GLOBAL`)
	r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
	require.NoError(t, e)
	defer r.Close()
	w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer w.Close()
	var saved RepairManifest
	m := NewMigrator(r, &failedHistoryIndexWriter{DaMengNativeWriter: w, index: "GlobalIndex"}, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, MaxParallel: 2, RepairSink: func(p RepairManifest) error { saved = p; return nil }})
	frozen = m.PreflightDamengNative(ctx)
	require.True(t, frozen.Ready, "%+v", frozen.Issues)
	deferred := false
	for _, o := range frozen.Objects {
		if o.Name == "GlobalIndex" {
			for _, u := range o.Units {
				require.Equal(t, "after_history", u.Phase)
				deferred = true
			}
		}
	}
	require.True(t, deferred)
	report := m.Run(ctx)
	require.True(t, report.HasFailures(), "索引故障必须保持任务失败")
	if report.HasFailures() {
		t.Logf("injected index errors: %+v; native verification: %+v", report.Indexes.Items, report.NativeSchema.Verification)
	}
	encoded, e := json.Marshal(saved)
	require.NoError(t, e)
	var reload RepairManifest
	require.NoError(t, json.Unmarshal(encoded, &reload))
	var cutoff string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME='Snapshot'`, dstSchema).Scan(&cutoff))
	historySucceeded, indexFailed := false, false
	for i, o := range reload.Objects {
		if o.Native == nil {
			continue
		}
		for j, u := range o.Native.Units {
			if u.Phase == "mv_history" {
				require.Equal(t, "succeeded", o.Units[j].Status)
				historySucceeded = true
			}
		}
		if o.Name != "GlobalIndex" {
			continue
		}
		for j := range o.Units {
			require.Equal(t, "failed", o.Units[j].Status)
			o.Units[j].Selected = true
		}
		units, e := target.ExecuteRepair(ctx, w, o, o.Units, nil)
		require.NoError(t, e)
		reload.Objects[i].Units = units
		indexFailed = true
	}
	require.True(t, historySucceeded)
	require.True(t, indexFailed)
	var after string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT TO_CHAR(LAST_REFRESH_DATE,'YYYY-MM-DD HH24:MI:SS.FF6') FROM SYS.USER_MVIEWS WHERE SCHID=(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$='SCH' AND NAME=?) AND MVIEW_NAME='Snapshot'`, dstSchema).Scan(&after))
	require.Equal(t, cutoff, after, "补迁索引不能重放已经成功的历史组")
	var value int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT "Value" FROM `+dq("Snapshot")).Scan(&value))
	require.Equal(t, 10, value)
	proof := damengnative.WithFinalizationVerifier(ctx, func(ctx context.Context) error {
		return VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects)
	})
	for i, o := range reload.Objects {
		if o.Native == nil || o.Native.Managed {
			continue
		}
		pending := false
		for j := range o.Units {
			o.Units[j].Selected = o.Units[j].Status != "succeeded" && o.Native.Units[j].Phase == "mv_restore"
			pending = pending || o.Units[j].Selected
		}
		if !pending {
			continue
		}
		units, e := target.ExecuteRepair(proof, w, o, o.Units, nil)
		require.NoError(t, e)
		reload.Objects[i].Units = units
	}
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects))
	exec(dst, "REFRESH MATERIALIZED VIEW "+dq("Snapshot")+" FAST")
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT "Value" FROM `+dq("Snapshot")).Scan(&value))
	require.Equal(t, 11, value)
}
