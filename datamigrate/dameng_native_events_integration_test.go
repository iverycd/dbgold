package datamigrate

import (
	"context"
	"database/sql"
	"os"
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
