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
	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

// The fixture creates only schema tables and rows. Its instance directories and
// server files must already exist on both sides, as required for real jobs.
func TestDamengNativeIntegrationBFileLocatorAndXMLType(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	directory, filename := os.Getenv("DAMENG_NATIVE_BFILE_DIRECTORY"), os.Getenv("DAMENG_NATIVE_BFILE_FILENAME")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" || directory == "" || filename == "" {
		t.Skip("BFILE/XMLTYPE 实库测试需要 DAMENG_NATIVE_SOURCE_DSN / SOURCE_SCHEMA / TARGET_DSN / TARGET_SCHEMA、ALLOW_DDL=1、BFILE_DIRECTORY / BFILE_FILENAME；两个独立空 Schema 及同名目录/文件须提前准备")
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
	sourceProfile, err := damengnative.ReadProfile(ctx, src, srcSchema)
	require.NoError(t, err)
	targetProfile, err := damengnative.ReadProfile(ctx, dst, dstSchema)
	require.NoError(t, err)
	require.False(t, sourceProfile.Instance == targetProfile.Instance && srcSchema == dstSchema)
	for _, pair := range []struct {
		db     *sql.DB
		schema string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var count int
		require.NoError(t, pair.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, pair.schema).Scan(&count))
		require.Zero(t, count, "测试 Schema 必须初始为空")
		require.NoError(t, damengnative.CheckBFileResource(ctx, pair.db, directory+":"+filename))
	}
	prefix := fmt.Sprintf("BFile%X", time.Now().UnixNano())
	name, retained := prefix+"Mixed", prefix+"TargetOnly"
	sq, dq, keep := damengnative.Qualified(srcSchema, name), damengnative.Qualified(dstSchema, name), damengnative.Qualified(dstSchema, retained)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = dst.ExecContext(cleanup, "DROP TABLE "+dq+" CASCADE CONSTRAINTS")
		_, _ = dst.ExecContext(cleanup, "DROP TABLE "+keep+" CASCADE CONSTRAINTS")
		_, _ = src.ExecContext(cleanup, "DROP TABLE "+sq+" CASCADE CONSTRAINTS")
	})
	exec := func(db *sql.DB, statement string, args ...any) {
		_, err := db.ExecContext(ctx, statement, args...)
		require.NoError(t, err)
	}
	exec(src, "CREATE TABLE "+sq+`("Id" INT PRIMARY KEY,"File" BFILE,"Mirror" BFILE,"Document" XMLTYPE,"Text" VARCHAR(100))`)
	locator := directory + ":" + filename
	smallXML := `<?xml version="1.0" encoding="UTF-8"?><root xmlns:q="urn:test" a="a&amp;b"><q:c><![CDATA[中文 < & 😀]]></q:c><!--keep--><empty/>  </root>`
	largeXML := "<root>" + strings.Repeat("中文😀&amp;", 20000) + "</root>"
	exec(src, "INSERT INTO "+sq+` VALUES(1,BFILENAME(?,?),BFILENAME(?,?),XMLTYPE(?),?)`, directory, filename, directory, filename, smallXML, "MiXeD\x00中文")
	exec(src, "INSERT INTO "+sq+` VALUES(2,NULL,NULL,NULL,NULL)`)
	exec(src, "INSERT INTO "+sq+` VALUES(3,BFILENAME(?,?),NULL,XMLTYPE(?),?)`, directory, filename, dm.NewClob(largeXML), "large XML")
	exec(src, "INSERT INTO "+sq+` VALUES(4,NULL,BFILENAME(?,?),XMLTYPE(?),?)`, directory, filename, "<empty/>", "same locator")
	missing := "DBGOLD_MISSING_" + prefix
	exec(dst, "CREATE TABLE "+keep+`("File" BFILE)`)
	exec(dst, "INSERT INTO "+keep+` VALUES(BFILENAME(?,?))`, directory, missing)
	r, err := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var frozen RepairManifest
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, PageSize: 1, RepairSink: func(plan RepairManifest) error { frozen = plan; return nil }})
	// An unprepared source resource is a real dependency failure, and must be
	// detected before any destination object from this plan can be created.
	exec(src, "INSERT INTO "+sq+` VALUES(999,BFILENAME(?,?),NULL,NULL,NULL)`, directory, missing)
	blocked := m.PreflightDamengNative(ctx)
	require.False(t, blocked.Ready)
	require.Contains(t, fmt.Sprint(blocked.Issues), "BFILE")
	var targetCount int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?`, dstSchema, name).Scan(&targetCount))
	require.Zero(t, targetCount)
	exec(src, "DELETE FROM "+sq+` WHERE "Id"=999`)
	preview := m.PreflightDamengNative(ctx)
	require.True(t, preview.Ready, "%+v", preview.Issues)
	var refs []damengnative.Reference
	for _, object := range preview.Objects {
		for _, ref := range object.External {
			if ref.Kind == "bfile" {
				refs = append(refs, ref)
			}
		}
	}
	require.Equal(t, []damengnative.Reference{{Kind: "bfile", Name: locator}}, refs, "相同定位器去重且目录不参与 Schema 映射")
	encoded, err := json.Marshal(preview)
	require.NoError(t, err)
	var reloaded damengnative.Preflight
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	var reloadedRefs []damengnative.Reference
	for _, object := range reloaded.Objects {
		for _, ref := range object.External {
			if ref.Kind == "bfile" {
				reloadedRefs = append(reloadedRefs, ref)
			}
		}
	}
	require.Equal(t, refs, reloadedRefs)
	report := m.Run(ctx)
	if report.HasFailures() {
		sourceState, sourceErr := r.SnapshotDamengNative(ctx)
		targetState, targetErr := w.InspectDamengNative(ctx)
		if sourceErr == nil && targetErr == nil {
			t.Logf("BFILE/XML native columns source=%+v target=%+v", sourceState.Tables, targetState.Tables)
		}
	}
	require.False(t, report.HasFailures(), "%+v / %+v", report.NativeSchema, report.DataValidation)
	require.NotNil(t, report.DataValidation)
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	require.NotEmpty(t, frozen.Objects, "完整可信计划必须持久化")
	encoded, err = json.Marshal(frozen)
	require.NoError(t, err)
	var restoredPlan RepairManifest
	require.NoError(t, json.Unmarshal(encoded, &restoredPlan))
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, restoredPlan.Objects), "重载的原生计划仍能完整核对当前对象、外部资源及数据")
	for id := 1; id <= 4; id++ {
		var sourceFile, targetFile, sourceMirror, targetMirror, sourceXML, targetXML, sourceText, targetText sql.NullString
		query := `SELECT "File","Mirror","Document","Text" FROM `
		require.NoError(t, src.QueryRowContext(ctx, query+sq+` WHERE "Id"=?`, id).Scan(&sourceFile, &sourceMirror, &sourceXML, &sourceText))
		require.NoError(t, dst.QueryRowContext(ctx, query+dq+` WHERE "Id"=?`, id).Scan(&targetFile, &targetMirror, &targetXML, &targetText))
		require.Equal(t, sourceFile, targetFile)
		require.Equal(t, sourceMirror, targetMirror)
		require.Equal(t, sourceXML, targetXML, "复制源存储的原始 XML；不能以重序列化替代")
		require.Equal(t, sourceText, targetText, "普通文本中的 NUL 不得清理")
	}
	var retainedFile string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT "File" FROM `+keep).Scan(&retainedFile))
	require.Equal(t, directory+":"+missing, retainedFile, "目标独有对象及其原始外部依赖保持不变")
	require.NoError(t, damengnative.CheckBFileResource(ctx, dst, locator))
}
