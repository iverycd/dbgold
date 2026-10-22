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
	"github.com/stretchr/testify/require"
)

// This test mutates only explicitly named, precreated, initially empty test
// schemas. Neither inherited credentials nor DSNs alone enable DDL. It never
// creates users/schemas or changes database compatibility settings.
func TestDamengNativeIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("未启用真实 DM8 验收：需要 DAMENG_NATIVE_SOURCE_DSN / SOURCE_SCHEMA / TARGET_DSN / TARGET_SCHEMA 及 DAMENG_NATIVE_ALLOW_DDL=1；必须使用预建的独立空测试 Schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", srcDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dst.Close() })
	require.NoError(t, src.PingContext(ctx))
	require.NoError(t, dst.PingContext(ctx))
	srcSchema, err = damengnative.ResolveSchema(ctx, src, srcSchema)
	require.NoError(t, err)
	dstSchema, err = damengnative.ResolveSchema(ctx, dst, dstSchema)
	require.NoError(t, err)
	srcProfile, err := damengnative.ReadProfile(ctx, src, srcSchema)
	require.NoError(t, err)
	dstProfile, err := damengnative.ReadProfile(ctx, dst, dstSchema)
	require.NoError(t, err)
	require.False(t, srcProfile.Instance == dstProfile.Instance && srcSchema == dstSchema, "禁止在同一实例的同一 Schema 上运行 DDL 验收")
	require.Equal(t, srcProfile.Version, dstProfile.Version, "源目标必须为相同 DM8 版本")
	require.Equal(t, srcProfile.Settings, dstProfile.Settings, "源目标兼容模式、字符集及语义参数必须相同")
	empty := func(db *sql.DB, schema string) {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON o.SCHID=s.ID WHERE s.TYPE$='SCH' AND s.NAME=?`, schema).Scan(&n))
		require.Zero(t, n, "测试 Schema %s 必须完全为空，不能只检查表", schema)
	}
	empty(src, srcSchema)
	empty(dst, dstSchema)

	prefix := fmt.Sprintf("Dmn%X", time.Now().UnixNano())
	parent, child, audit, ident := prefix+"Mixed", prefix+"Child", prefix+"Audit", prefix+"Identity"
	seq, view, fn, proc, pkg, trig := prefix+"Seq", prefix+"View", prefix+"Fn", prefix+"Proc", prefix+"Pkg", prefix+"Trigger"
	typ, typed, mv, syn, marker := prefix+"Type", prefix+"Typed", prefix+"MV", prefix+"Syn", prefix+"TargetOnly"
	q := damengnative.Quote
	sq := func(name string) string { return damengnative.Qualified(srcSchema, name) }
	dq := func(name string) string { return damengnative.Qualified(dstSchema, name) }
	storage := "DBGOLD_MV_" + damengnative.Fingerprint([]string{srcSchema, mv})[:20]
	// Cleanup names are fixed by this fixture. Never DROP SCHEMA or enumerate and
	// delete objects that another process may have created during the run.
	cleanup := func(db *sql.DB, qualify func(string) string) {
		clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, statement := range []string{
			"DROP MATERIALIZED VIEW " + qualify(mv), "DROP VIEW " + qualify(view), "DROP SYNONYM " + qualify(syn),
			"DROP TRIGGER " + qualify(trig), "DROP PACKAGE " + qualify(pkg), "DROP FUNCTION " + qualify(fn), "DROP PROCEDURE " + qualify(proc),
			"DROP TABLE " + qualify(child) + " CASCADE CONSTRAINTS", "DROP TABLE " + qualify(typed) + " CASCADE CONSTRAINTS",
			"DROP TABLE " + qualify(parent) + " CASCADE CONSTRAINTS", "DROP TABLE " + qualify(audit) + " CASCADE CONSTRAINTS",
			"DROP TABLE " + qualify(ident) + " CASCADE CONSTRAINTS", "DROP TABLE " + qualify(marker) + " CASCADE CONSTRAINTS",
			"DROP TABLE " + qualify(storage) + " CASCADE CONSTRAINTS", "DROP TYPE " + qualify(typ), "DROP SEQUENCE " + qualify(seq),
		} {
			_, _ = db.ExecContext(clean, statement)
		}
	}
	t.Cleanup(func() { cleanup(dst, dq); cleanup(src, sq) })
	exec := func(db *sql.DB, statement string) {
		_, e := db.ExecContext(ctx, statement)
		require.NoError(t, e, "fixture SQL: %s", statement)
	}
	sourceExec := func(statement string) { exec(src, statement) }
	count := func(db *sql.DB, table string) int64 {
		var n int64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n))
		return n
	}
	sourceExec("CREATE TABLE " + sq(parent) + ` ("Id" INT NOT NULL, "MixedText" VARCHAR(100), "Amount" DECIMAL(38,18), "Payload" BLOB, "ShortRaw" VARBINARY(16), "Memo" CLOB, "Stamp" TIMESTAMP(6), CONSTRAINT ` + q(prefix+"PK") + ` PRIMARY KEY ("Id"), CONSTRAINT ` + q(prefix+"CK") + ` CHECK ("Id">0))`)
	sourceExec("CREATE TABLE " + sq(audit) + ` ("Id" INT NOT NULL)`)
	sourceExec("CREATE OR REPLACE TRIGGER " + sq(trig) + " AFTER INSERT ON " + sq(parent) + ` FOR EACH ROW BEGIN INSERT INTO ` + sq(audit) + ` ("Id") VALUES (:NEW."Id"); END;`)
	sourceExec("INSERT INTO " + sq(parent) + ` VALUES (1,'Keep'||CHR(0)||'AbC中文',12345678901234567890.123456789012345678,HEXTORAW('00017FFF80AA'),HEXTORAW('00FF01'),TO_CLOB('Memo MiXeD 中文'),TO_TIMESTAMP('2024-02-29 12:34:56.123456','YYYY-MM-DD HH24:MI:SS.FF6'))`)
	sourceExec("INSERT INTO " + sq(parent) + ` ("Id","MixedText","Amount") VALUES (2,'Second',-12345678901234567890.123456789012345678)`)
	sourceExec("CREATE INDEX " + sq(prefix+"IX") + " ON " + sq(parent) + ` ("MixedText" DESC)`)
	sourceExec("COMMENT ON TABLE " + sq(parent) + " IS 'DM8 MiXeD 表注释'")
	sourceExec("COMMENT ON COLUMN " + sq(parent) + `."MixedText" IS 'DM8 MiXeD 列注释'`)
	sourceExec("CREATE TABLE " + sq(child) + ` ("Id" INT PRIMARY KEY, "ParentId" INT NOT NULL, CONSTRAINT ` + q(prefix+"FK") + ` FOREIGN KEY ("ParentId") REFERENCES ` + sq(parent) + ` ("Id"))`)
	sourceExec("INSERT INTO " + sq(child) + ` VALUES (1,1)`)
	sourceExec("CREATE TABLE " + sq(ident) + ` ("Id" BIGINT IDENTITY(10,3) PRIMARY KEY, "Note" VARCHAR(32))`)
	sourceExec("INSERT INTO " + sq(ident) + ` ("Note") VALUES ('keep')`)
	sourceExec("INSERT INTO " + sq(ident) + ` ("Note") VALUES ('delete highest')`)
	sourceExec("DELETE FROM " + sq(ident) + ` WHERE "Id"=13`)
	sourceExec("CREATE SEQUENCE " + sq(seq) + " START WITH 50 INCREMENT BY 7 NOCACHE NOCYCLE")
	for i := 0; i < 2; i++ {
		var value int64
		require.NoError(t, src.QueryRowContext(ctx, "SELECT "+sq(seq)+".NEXTVAL FROM DUAL").Scan(&value))
	}
	sourceExec("CREATE VIEW " + sq(view) + ` AS SELECT "Id","MixedText" FROM ` + sq(parent))
	sourceExec("CREATE OR REPLACE FUNCTION " + sq(fn) + `(P_VAL INT) RETURN INT AS BEGIN RETURN P_VAL+1; END;`)
	sourceExec("CREATE OR REPLACE PROCEDURE " + sq(proc) + ` AS BEGIN NULL; END;`)
	sourceExec("CREATE OR REPLACE PACKAGE " + sq(pkg) + ` AS FUNCTION ADDONE(P_VAL INT) RETURN INT; END;`)
	sourceExec("CREATE OR REPLACE PACKAGE BODY " + sq(pkg) + ` AS FUNCTION ADDONE(P_VAL INT) RETURN INT AS BEGIN RETURN P_VAL+1; END; END;`)
	// TYPE and COMPLETE/ON DEMAND syntax follows the official DM8 SQL manual.
	sourceExec("CREATE TYPE " + sq(typ) + ` AS OBJECT ("Value" INT)`)
	sourceExec("CREATE TABLE " + sq(typed) + ` ("Id" INT PRIMARY KEY, "Obj" ` + sq(typ) + `)`)
	sourceExec("INSERT INTO " + sq(typed) + " VALUES (1," + sq(typ) + "(42))")
	sourceExec("CREATE MATERIALIZED VIEW " + sq(mv) + ` REFRESH COMPLETE ON DEMAND AS SELECT "Id","MixedText" FROM ` + sq(parent))
	// Leave the MV deliberately stale. Migration must preserve its stored rows,
	// rather than silently refreshing it from the now-larger base table.
	sourceExec("INSERT INTO " + sq(parent) + ` ("Id","MixedText") VALUES (3,'after MV snapshot')`)
	sourceExec("CREATE SYNONYM " + sq(syn) + " FOR " + sq(parent))
	sourceExec("GRANT SELECT ON " + sq(parent) + " TO PUBLIC")
	sourceExec("GRANT UPDATE(\"MixedText\") ON " + sq(parent) + " TO PUBLIC")
	exec(dst, "CREATE TABLE "+dq(marker)+` ("Sentinel" INT)`)
	exec(dst, "INSERT INTO "+dq(marker)+" VALUES (9876)")
	require.EqualValues(t, 3, count(src, sq(audit)))
	var sourceBoundary string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT CAST(LAST_NUMBER AS VARCHAR(80)) FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?`, srcSchema, seq).Scan(&sourceBoundary))

	run := func(overwrite bool) (MigrationReport, *damengnative.Preflight) {
		r, e := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
		require.NoError(t, e)
		defer r.Close()
		w, e := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
		require.NoError(t, e)
		defer w.Close()
		job := &Job{LogCh: make(chan string, 4096)}
		m := NewMigrator(r, w, job, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, OverwriteExisting: overwrite, MigrateTriggers: true, CaptureRepair: true, PageSize: 1, MaxParallel: 2})
		preview := m.PreflightDamengNative(ctx)
		report := m.Run(ctx)
		if report.HasFailures() {
			encoded, _ := json.Marshal(report)
			t.Logf("native report: %s", encoded)
			t.Logf("native logs: %v", drainLogs(job))
		}
		return report, preview
	}
	report, preview := run(false)
	require.True(t, preview.Ready, "%+v", preview.Issues)
	require.Empty(t, preview.Drops)
	require.False(t, report.HasFailures(), "%+v", report.NativeSchema)
	require.NotNil(t, report.NativeSchema)
	var afterBoundary string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT CAST(LAST_NUMBER AS VARCHAR(80)) FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME=?`, srcSchema, seq).Scan(&afterBoundary))
	require.Equal(t, sourceBoundary, afterBoundary, "预检和迁移不能消费源序列")

	verify := func() {
		require.EqualValues(t, 3, count(dst, dq(parent)))
		require.EqualValues(t, 3, count(dst, dq(audit)), "数据复制不能触发业务触发器产生额外审计行")
		require.EqualValues(t, 1, count(dst, dq(child)))
		require.EqualValues(t, 1, count(dst, dq(ident)))
		require.EqualValues(t, 2, count(dst, dq(mv)), "必须保留旧物化视图快照")
		var text, amount, memo, stamp string
		var payload, raw []byte
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT "MixedText",CAST("Amount" AS VARCHAR(90)),"Payload","ShortRaw","Memo",TO_CHAR("Stamp",'YYYY-MM-DD HH24:MI:SS.FF6') FROM `+dq(parent)+` WHERE "Id"=1`).Scan(&text, &amount, &payload, &raw, &memo, &stamp))
		require.Equal(t, "Keep\x00AbC中文", text)
		require.Equal(t, "12345678901234567890.123456789012345678", amount)
		require.Equal(t, []byte{0, 1, 0x7f, 0xff, 0x80, 0xaa}, payload)
		require.Equal(t, []byte{0, 0xff, 1}, raw)
		require.Equal(t, "Memo MiXeD 中文", memo)
		require.Equal(t, "2024-02-29 12:34:56.123456", stamp)
		var comment string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT COMMENT$ FROM SYS.SYSCOLUMNCOMMENTS WHERE SCHNAME=? AND TVNAME=? AND COLNAME='MixedText'`, dstSchema, parent).Scan(&comment))
		require.Equal(t, "DM8 MiXeD 列注释", comment)
		var value int
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT "+dq(fn)+"(7) FROM DUAL").Scan(&value))
		require.Equal(t, 8, value)
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT "+dq(pkg)+".ADDONE(8) FROM DUAL").Scan(&value))
		require.Equal(t, 9, value)
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT T."Obj"."Value" FROM `+dq(typed)+" T WHERE T.\"Id\"=1").Scan(&value))
		require.Equal(t, 42, value)
		require.EqualValues(t, 3, count(dst, dq(syn)))
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Sentinel\" FROM "+dq(marker)).Scan(&value))
		require.Equal(t, 9876, value)
	}
	verify()
	var targetNext string
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT CAST("+dq(seq)+".NEXTVAL AS VARCHAR(80)) FROM DUAL").Scan(&targetNext))
	require.Equal(t, strings.TrimSpace(sourceBoundary), strings.TrimSpace(targetNext), "目标序列从源冻结边界续号")
	exec(dst, "INSERT INTO "+dq(ident)+` ("Note") VALUES ('target next')`)
	var identity int64
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Id\" FROM "+dq(ident)+` WHERE "Note"='target next'`).Scan(&identity))
	require.EqualValues(t, 16, identity, "删除源最大 ID 后仍保留实际自增进度")
	exec(dst, "INSERT INTO "+dq(parent)+` ("Id","MixedText") VALUES (4,'target write after migration')`)
	require.EqualValues(t, 4, count(dst, dq(audit)), "迁移完成后触发器应可执行")

	blocked, conflicts := run(false)
	require.False(t, conflicts.Ready)
	require.NotEmpty(t, conflicts.Conflicts)
	require.Empty(t, conflicts.Drops)
	require.True(t, blocked.HasFailures())
	require.Zero(t, blocked.Data.Total)
	require.EqualValues(t, 4, count(dst, dq(parent)), "关闭覆盖时整次任务不能修改目标")
	require.EqualValues(t, 4, count(dst, dq(audit)))
	replaced, replacement := run(true)
	require.True(t, replacement.Ready, "%+v", replacement.Issues)
	require.NotEmpty(t, replacement.Drops)
	for _, drop := range replacement.Drops {
		require.NotEqual(t, marker, drop.Name, "不能删除目标独有对象")
	}
	require.False(t, replaced.HasFailures(), "%+v", replaced.NativeSchema)
	verify()
	require.EqualValues(t, 3, count(src, sq(audit)), "整个迁移过程不能写源业务表")
}
