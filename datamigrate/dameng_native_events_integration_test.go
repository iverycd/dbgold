package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

type eventFinalizationProofWriter struct {
	*target.DaMengNativeWriter
	proof func(context.Context) error
}

func (w *eventFinalizationProofWriter) WithDamengNativeEventFinalization(ctx context.Context, activate func(context.Context) error) error {
	if err := w.proof(ctx); err != nil {
		return err
	}
	return w.DaMengNativeWriter.WithDamengNativeEventFinalization(ctx, activate)
}

func nativeEventIntegrationDB(t *testing.T, dsn, schema string) (*sql.DB, string) {
	t.Helper()
	db, err := sql.Open("dm", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	schema, err = damengnative.ResolveSchema(ctx, db, schema)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.NAME=?`, schema).Scan(&count))
	require.Zero(t, count, "事件验收仅允许使用预建的独立空 Schema %s", schema)
	return db, schema
}

func nativeEventIntegrationCleanup(t *testing.T, db *sql.DB, schema string, tables ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, damengnative.EventTriggerStateSQL(schema, "EventHook", false))
		_, _ = db.ExecContext(ctx, "DROP TRIGGER "+damengnative.Qualified(schema, "EventHook"))
		for _, table := range tables {
			_, _ = db.ExecContext(ctx, "DROP TABLE "+damengnative.Qualified(schema, table))
		}
	})
}

func TestDamengNativeEventLifecycleIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_NATIVE_EVENT_DSN"), os.Getenv("DAMENG_NATIVE_EVENT_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("事件生命周期真实验收需要 DAMENG_NATIVE_EVENT_DSN / EVENT_SCHEMA 与 DAMENG_NATIVE_ALLOW_DDL=1；Schema 必须预建且为空")
	}
	db, schema := nativeEventIntegrationDB(t, dsn, schema)
	nativeEventIntegrationCleanup(t, db, schema, "After", "During", "EventAudit")
	q := func(name string) string { return damengnative.Qualified(schema, name) }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exec := func(sql string) { _, err := db.ExecContext(ctx, sql); require.NoError(t, err) }
	exec("CREATE TABLE " + q("EventAudit") + "(K VARCHAR(20))")
	exec("CREATE TRIGGER " + q("EventHook") + " AFTER DDL ON " + damengnative.Quote(schema) + ".SCHEMA BEGIN INSERT INTO " + q("EventAudit") + " VALUES('DDL'); END;")
	r, err := source.NewDaMengNative(dsn, schema, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	snapshot, err := r.SnapshotDamengNative(ctx)
	require.NoError(t, err)
	require.False(t, damengnative.HasErrors(snapshot.Issues), "%+v", snapshot.Issues)
	var hook damengnative.Object
	for _, object := range snapshot.Objects {
		if object.Name == "EventHook" {
			hook = object
		}
	}
	require.True(t, hook.EventTrigger)
	require.Equal(t, "ENABLED", hook.Status)
	require.Len(t, hook.Units, 2)
	exec(damengnative.EventTriggerStateSQL(schema, hook.Name, false))
	exec("DROP TRIGGER " + q(hook.Name))
	w, err := target.NewDaMengNative(dsn, schema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	require.NoError(t, w.InstallDamengNativeEvent(ctx, hook, hook.Units[0]))
	exec("CREATE TABLE " + q("During") + "(C INT)")
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("EventAudit")).Scan(&count))
	require.Zero(t, count, "安装、自身 CREATE 和导入窗口内 DDL 不能运行事件业务逻辑")
	verify := *snapshot
	verify.Objects = append([]damengnative.Object{}, snapshot.Objects...)
	for i := range verify.Objects {
		if verify.Objects[i].EventTrigger {
			verify.Objects[i].Status = "DISABLED"
		}
	}
	issues, err := w.VerifyDamengNative(ctx, &verify)
	require.NoError(t, err)
	require.False(t, damengnative.HasErrors(issues), "%+v", issues)
	require.NoError(t, w.WithDamengNativeEventFinalization(ctx, func(ctx context.Context) error {
		return w.SetDamengNativeEventState(ctx, hook, hook.Units[1])
	}))
	exec("CREATE TABLE " + q("After") + "(C INT)")
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("EventAudit")).Scan(&count))
	require.Equal(t, 1, count, "恢复后明确的业务 DDL 应按原定义触发")
}

func TestDamengNativeEventMigrationIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_EVENT_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_EVENT_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_EVENT_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_EVENT_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || srcDSN == "" || srcSchema == "" || dstDSN == "" || dstSchema == "" {
		t.Skip("事件完整迁移真实验收需要 DAMENG_NATIVE_EVENT_SOURCE_DSN / SOURCE_SCHEMA / TARGET_DSN / TARGET_SCHEMA 与 DAMENG_NATIVE_ALLOW_DDL=1；两个 Schema 必须预建、独立且为空")
	}
	src, srcSchema := nativeEventIntegrationDB(t, srcDSN, srcSchema)
	dst, dstSchema := nativeEventIntegrationDB(t, dstDSN, dstSchema)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sp, err := damengnative.ReadProfile(ctx, src, srcSchema)
	require.NoError(t, err)
	dp, err := damengnative.ReadProfile(ctx, dst, dstSchema)
	require.NoError(t, err)
	require.False(t, sp.Instance == dp.Instance && srcSchema == dstSchema, "同一 Schema 不能同时作为源和目标")
	nativeEventIntegrationCleanup(t, src, srcSchema, "After", "Rows", "EventAudit")
	nativeEventIntegrationCleanup(t, dst, dstSchema, "After", "Rows", "EventAudit")
	sq := func(name string) string { return damengnative.Qualified(srcSchema, name) }
	dq := func(name string) string { return damengnative.Qualified(dstSchema, name) }
	exec := func(db *sql.DB, sql string) { _, err := db.ExecContext(ctx, sql); require.NoError(t, err) }
	exec(src, "CREATE TABLE "+sq("EventAudit")+"(K VARCHAR(20))")
	exec(src, "CREATE TABLE "+sq("Rows")+`("Id" INT PRIMARY KEY,"Text" VARCHAR(100),"Amount" DECIMAL(38,18),"Raw" VARBINARY(20))`)
	exec(src, "INSERT INTO "+sq("Rows")+` VALUES(1,'Aa'||CHR(0)||'中文',12345678901234567890.123456789012345678,HEXTORAW('0001FF80'))`)
	exec(src, "CREATE TRIGGER "+sq("EventHook")+" AFTER DDL ON "+damengnative.Quote(srcSchema)+".SCHEMA BEGIN INSERT INTO "+sq("EventAudit")+" VALUES('DDL'); END;")
	r, err := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var saved RepairManifest
	cfg := Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, MigrateTriggers: true, ValidateData: true, RepairSink: func(manifest RepairManifest) error { saved = manifest; return nil }}
	proofCalls := 0
	checked := &eventFinalizationProofWriter{DaMengNativeWriter: w, proof: func(proof context.Context) error {
		proofCalls++
		if err := NativeRepairFinalizationReady(saved.Objects, ""); err != nil {
			return err
		}
		return VerifyNativeRepairFinalization(proof, r, w, cfg, saved.Objects)
	}}
	m := NewMigrator(r, checked, &Job{LogCh: make(chan string, 4096)}, cfg)
	preflight := m.PreflightDamengNative(ctx)
	require.True(t, preflight.Ready, "%+v", preflight.Issues)
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "native verification=%+v data=%+v logs=%v", report.NativeSchema.Verification, report.DataValidation, drainLogs(m.job))
	require.NotNil(t, report.DataValidation)
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	require.Equal(t, 1, proofCalls, "真实数据应可通过恢复路径的整任务目录/内容核验")
	var state string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME='EventHook'`, dstSchema).Scan(&state))
	require.Equal(t, "Y", state)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")).Scan(&count))
	require.Zero(t, count, "全部迁移 DDL/编译/核验期间事件逻辑必须保持禁用")
	exec(dst, "CREATE TABLE "+dq("After")+"(C INT)")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+sq("EventAudit")).Scan(&count))
	require.Zero(t, count, "整个迁移不能执行源端 DDL")
}

// Background hooks are installed disabled atomically even while the target
// checkpoint dispatcher is enabled. This test never changes instance settings;
// its dedicated fixture must be configured with ENABLE_CKPT_EVENT_TRIG=1.
func TestDamengNativeBackgroundEventMigrationIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_EVENT_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_EVENT_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_EVENT_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_EVENT_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || srcDSN == "" || srcSchema == "" || dstDSN == "" || dstSchema == "" {
		t.Skip("后台事件完整迁移验收需要专用、预建且为空的 EVENT_SOURCE/TARGET Schema、DSN 和 DAMENG_NATIVE_ALLOW_DDL=1；目标 CHECKPOINT 调度必须预先开启")
	}
	src, srcSchema := nativeEventIntegrationDB(t, srcDSN, srcSchema)
	dst, dstSchema := nativeEventIntegrationDB(t, dstDSN, dstSchema)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var checkpointDispatch string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='ENABLE_CKPT_EVENT_TRIG'`).Scan(&checkpointDispatch))
	require.Equal(t, "1", strings.TrimSpace(checkpointDispatch), "专用目标实例必须在测试前开启 CHECKPOINT 调度；测试不会修改实例参数")
	names := []string{"TimerOn", "TimerOff", "CheckpointOn", "CheckpointOff"}
	for _, fixture := range []struct {
		db     *sql.DB
		schema string
	}{{src, srcSchema}, {dst, dstSchema}} {
		fixture := fixture
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			for _, name := range names {
				_, _ = fixture.db.ExecContext(cleanup, damengnative.EventTriggerStateSQL(fixture.schema, name, false))
				_, _ = fixture.db.ExecContext(cleanup, "DROP TRIGGER "+damengnative.Qualified(fixture.schema, name))
			}
			for _, name := range []string{"Rows", "EventAudit", "Control"} {
				_, _ = fixture.db.ExecContext(cleanup, "DROP TABLE "+damengnative.Qualified(fixture.schema, name))
			}
		})
	}
	sq := func(name string) string { return damengnative.Qualified(srcSchema, name) }
	dq := func(name string) string { return damengnative.Qualified(dstSchema, name) }
	exec := func(db *sql.DB, statement string) { _, err := db.ExecContext(ctx, statement); require.NoError(t, err) }
	exec(src, "CREATE TABLE "+sq("Control")+`("Flag" INT)`)
	exec(src, "INSERT INTO "+sq("Control")+" VALUES(0)")
	exec(src, "CREATE TABLE "+sq("EventAudit")+`("Kind" VARCHAR(30))`)
	exec(src, "CREATE TABLE "+sq("Rows")+`("Id" INT,"Text" VARCHAR(100))`)
	exec(src, "INSERT INTO "+sq("Rows")+` VALUES(1,'原始快照'||CHR(0)||'Mixed')`)
	for _, name := range names {
		header := "CHECKPOINT ON DATABASE"
		if strings.HasPrefix(name, "Timer") {
			header = "TIMER ON DATABASE FOR EACH 1 DAY FOR EACH 1 MINUTE"
		}
		state := ""
		if strings.HasSuffix(name, "Off") {
			state = "DISABLE "
		}
		exec(src, "CREATE TRIGGER "+sq(name)+" AFTER "+header+" WHEN(1=1) "+state+`DECLARE RUN_FLAG INT; BEGIN SELECT "Flag" INTO RUN_FLAG FROM `+sq("Control")+`; IF RUN_FLAG=1 THEN INSERT INTO `+sq("EventAudit")+` VALUES('`+name+`'); END IF; END;`)
	}
	r, err := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var saved RepairManifest
	cfg := Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, MigrateTriggers: true, ValidateData: true, RepairSink: func(manifest RepairManifest) error { saved = manifest; return nil }}
	proofCalls := 0
	checked := &eventFinalizationProofWriter{DaMengNativeWriter: w, proof: func(proof context.Context) error {
		proofCalls++
		for _, name := range names {
			var state string
			if err := dst.QueryRowContext(proof, `SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, dstSchema, name).Scan(&state); err != nil {
				return err
			}
			if state != "N" {
				return fmt.Errorf("后台事件 %s 在最终切换前已启用: %s", name, state)
			}
		}
		// An older durable plan did not include the atomic DISABLE header.
		// An uncertain result must reconcile the server's disabled definition
		// and automatic OR REPLACE prefix, without replaying CREATE.
		legacyObjects := slices.Clone(saved.Objects)
		for index, object := range legacyObjects {
			if object.Name != "TimerOn" {
				continue
			}
			legacy := object
			native := *object.Native
			native.Units = slices.Clone(native.Units)
			native.Units[0].SQL = strings.Replace(native.Units[0].SQL, "DISABLE DECLARE", "DECLARE", 1)
			legacy.Native = &native
			legacy.NativeFingerprint = damengnative.Fingerprint(native)
			legacy.Units = slices.Clone(object.Units)
			for i := range legacy.Units {
				legacy.Units[i].Selected = false
			}
			legacy.Units[0].SQL, legacy.Units[0].OriginalSQL = native.Units[0].SQL, native.Units[0].SQL
			legacy.Units[0].Status, legacy.Units[0].Selected = "unknown", true
			out, err := target.ExecuteRepair(proof, w, legacy, legacy.Units, nil)
			if err != nil {
				return err
			}
			if out[0].Status != "succeeded" {
				return fmt.Errorf("旧事件计划的未知 CREATE 结果未被目录对账: %s", out[0].Status)
			}
			legacy.Units = out
			legacyObjects[index] = legacy
		}
		if err := NativeRepairFinalizationReady(legacyObjects, ""); err != nil {
			return err
		}
		return VerifyNativeRepairFinalization(proof, r, w, cfg, legacyObjects)
	}}
	m := NewMigrator(r, checked, &Job{LogCh: make(chan string, 4096)}, cfg)
	preflight := m.PreflightDamengNative(ctx)
	require.True(t, preflight.Ready, "%+v", preflight.Issues)
	report := m.Run(ctx)
	require.False(t, report.HasFailures(), "native=%+v data=%+v logs=%v", report.NativeSchema.Verification, report.DataValidation, drainLogs(m.job))
	require.NotNil(t, report.DataValidation)
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	require.Equal(t, 1, proofCalls)
	for _, name := range names {
		var state string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT STATUS FROM ALL_TRIGGERS WHERE OWNER=? AND TRIGGER_NAME=?`, dstSchema, name).Scan(&state))
		want := "Y"
		if strings.HasSuffix(name, "Off") {
			want = "N"
		}
		require.Equal(t, want, state, name)
	}
	var audit int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")).Scan(&audit))
	require.Zero(t, audit)
	// Cutover is complete. Subsequent business events must use the restored
	// original hook bodies, and the originally disabled hooks must stay inert.
	exec(dst, "UPDATE "+dq("Control")+` SET "Flag"=1`)
	exec(dst, "CALL CHECKPOINT(100)")
	// CHECKPOINT returns before its asynchronous event worker commits.
	require.Eventually(t, func() bool {
		err := dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")+` WHERE "Kind"='CheckpointOn'`).Scan(&audit)
		return err == nil && audit > 0
	}, 10*time.Second, 100*time.Millisecond, "原启用 CHECKPOINT 在切换后必须执行原业务体")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")+` WHERE "Kind" IN ('CheckpointOff','TimerOff')`).Scan(&audit))
	require.Zero(t, audit)
	if os.Getenv("DAMENG_NATIVE_BACKGROUND_WAIT_TIMER") == "1" {
		deadline := time.NewTimer(130 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
	wait:
		for {
			select {
			case <-tick.C:
				require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")+` WHERE "Kind"='TimerOn'`).Scan(&audit))
				if audit > 0 {
					break wait
				}
			case <-deadline.C:
				t.Fatal("原启用 TIMER 在切换后未按原调度执行")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("EventAudit")+` WHERE "Kind"='TimerOff'`).Scan(&audit))
		require.Zero(t, audit)
	}
	require.NoError(t, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+sq("EventAudit")).Scan(&audit))
	require.Zero(t, audit, "源快照与停写数据不得在验收中被后台事件改写")
}
