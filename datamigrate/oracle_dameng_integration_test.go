package datamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// This opt-in test creates fixtures only in explicitly named, initially empty
// schemas. ALLOW_DDL is independent of DSN presence so inherited credentials can
// never accidentally enable it. No database compatibility parameter is changed.
func TestOracleDamengIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("ORACLE_DM_TEST_SOURCE_DSN"), os.Getenv("ORACLE_DM_TEST_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("ORACLE_DM_TEST_TARGET_DSN"), os.Getenv("ORACLE_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("ORACLE_DM_TEST_ALLOW_DDL") != "1" || srcDSN == "" || srcSchema == "" || dstDSN == "" || dstSchema == "" {
		t.Skip("需要显式配置 ORACLE_DM_TEST_SOURCE_DSN / SOURCE_SCHEMA / TARGET_DSN / TARGET_SCHEMA，及 ORACLE_DM_TEST_ALLOW_DDL=1；仅用于独立空测试 schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src, err := sql.Open("oracle", srcDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dst.Close() })
	require.NoError(t, src.PingContext(ctx))
	require.NoError(t, dst.PingContext(ctx))
	srcSchema, dstSchema = strings.ToUpper(srcSchema), strings.ToUpper(dstSchema)
	var tables int
	require.NoError(t, src.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=:1", srcSchema).Scan(&tables))
	require.Zero(t, tables, "Oracle 测试 schema 必须初始为空")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=?", dstSchema).Scan(&tables))
	require.Zero(t, tables, "达梦测试 schema 必须初始为空")
	var compatible, timeMode string
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='COMPATIBLE_MODE'").Scan(&compatible))
	compatible = strings.TrimSpace(compatible)
	if expected := os.Getenv("ORACLE_DM_TEST_EXPECT_MODE"); expected != "" {
		require.Equal(t, expected, compatible, "目标实例模式与本轮验收矩阵不一致")
	}
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME='IFUN_DATETIME_MODE'").Scan(&timeMode))
	t.Logf("DM 实际 COMPATIBLE_MODE=%s, IFUN_DATETIME_MODE=%s；测试不修改参数", compatible, strings.TrimSpace(timeMode))

	prefix := fmt.Sprintf("ODM%X", time.Now().UnixNano())
	parent, child, seq, trigger, view, index, fk, high := prefix+"P", prefix+"C", prefix+"S", prefix+"TR", prefix+"V", prefix+"IX", prefix+"FK", prefix+"TS"
	longTable, locatorTable, xmlTable, bfileTable, spatialTable := prefix+"L", prefix+"R", prefix+"X", prefix+"BF", prefix+"G"
	quote := func(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }
	sq := func(n string) string { return quote(srcSchema) + "." + quote(n) }
	dq := func(n string) string { return dialect.NewDaMeng().QualifyTable(dstSchema, n) }
	cleanTarget := func(cleanup context.Context) {
		for _, statement := range []string{"DROP VIEW " + dq(view), "DROP TABLE " + dq(child) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(parent) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(high) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(longTable) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(locatorTable) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(xmlTable) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(bfileTable) + " CASCADE CONSTRAINTS", "DROP TABLE " + dq(spatialTable) + " CASCADE CONSTRAINTS", "DROP SEQUENCE " + dq(seq)} {
			_, _ = dst.ExecContext(cleanup, statement)
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		cleanTarget(cleanup)
		for _, statement := range []string{"DROP VIEW " + sq(view), "DROP TABLE " + sq(child) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(parent) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(high) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(longTable) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(locatorTable) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(xmlTable) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(bfileTable) + " CASCADE CONSTRAINTS PURGE", "DROP TABLE " + sq(spatialTable) + " CASCADE CONSTRAINTS PURGE", "DROP SEQUENCE " + sq(seq)} {
			_, _ = src.ExecContext(cleanup, statement)
		}
	})
	execSource := func(statement string) {
		_, e := src.ExecContext(ctx, statement)
		require.NoError(t, e)
	}
	execSource("CREATE TABLE " + sq(parent) + ` (
	 ID NUMBER(18) NOT NULL PRIMARY KEY,
	 NAME VARCHAR2(80 CHAR) DEFAULT 'AbC中文',
	 AMOUNT NUMBER(38,18),
	 NNAME NCHAR(8), VNAME NVARCHAR2(20),
	 FLOAT_VAL FLOAT(126), BINARY_VAL BINARY_FLOAT, DOUBLE_VAL BINARY_DOUBLE,
	 AT_DATE DATE,
	 AT_TS TIMESTAMP(6),
	 AT_LTZ TIMESTAMP(6) WITH LOCAL TIME ZONE,
	 DUR INTERVAL DAY(3) TO SECOND(6),
	 MONTHS_VAL INTERVAL YEAR(3) TO MONTH,
	 BODY CLOB, NBODY NCLOB, PAYLOAD BLOB
	)`)
	execSource("CREATE SEQUENCE " + sq(seq) + " START WITH 100 INCREMENT BY 5 MINVALUE 1 MAXVALUE 1000000000 NOCYCLE NOCACHE")
	execSource("CREATE OR REPLACE TRIGGER " + sq(trigger) + " BEFORE INSERT ON " + sq(parent) + ` FOR EACH ROW BEGIN IF :NEW.ID IS NULL THEN SELECT ` + sq(seq) + `.NEXTVAL INTO :NEW.ID FROM DUAL; END IF; END;`)
	execSource("INSERT INTO " + sq(parent) + ` (ID,NAME,AMOUNT,AT_DATE,AT_TS,DUR,MONTHS_VAL,BODY,NBODY,PAYLOAD) VALUES
	(1,'AbC中文',12345678901234567890.123456789012345678,
	TO_DATE('2024-02-29 23:34:56','YYYY-MM-DD HH24:MI:SS'),
	TO_TIMESTAMP('2024-02-29 23:34:56.123456','YYYY-MM-DD HH24:MI:SS.FF6'),
	TO_DSINTERVAL('-001 02:03:04.123456'),TO_YMINTERVAL('-001-02'),
	TO_CLOB('保留AbC中文'),TO_NCLOB(N'保留大小写AbC'),HEXTORAW('00AAFF'))`)
	execSource("INSERT INTO " + sq(parent) + ` (NAME,BODY,NBODY,PAYLOAD) VALUES ('sequence row',EMPTY_CLOB(),EMPTY_CLOB(),EMPTY_BLOB())`)
	execSource("UPDATE " + sq(parent) + ` SET NNAME=N'中文AbC',VNAME=N'中文Mixed',
	 FLOAT_VAL=12345678901234567890.123456789,
	 BINARY_VAL=TO_BINARY_FLOAT('16777217'),DOUBLE_VAL=TO_BINARY_DOUBLE('1.2345678901234567'),
	 AT_LTZ=TO_TIMESTAMP_TZ('2024-02-29 23:34:56.123456 -05:00','YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM') WHERE ID=1`)
	execSource("CREATE INDEX " + sq(index) + " ON " + sq(parent) + " (NAME DESC)")
	execSource("COMMENT ON TABLE " + sq(parent) + " IS 'Oracle到达梦测试AbC'")
	execSource("COMMENT ON COLUMN " + sq(parent) + ".NAME IS '列注释AbC'")
	execSource("CREATE TABLE " + sq(child) + " (ID NUMBER(18) PRIMARY KEY, PARENT_ID NUMBER(18) NOT NULL, CONSTRAINT " + quote(fk) + " FOREIGN KEY (PARENT_ID) REFERENCES " + sq(parent) + " (ID) ON DELETE CASCADE)")
	execSource("INSERT INTO " + sq(child) + " VALUES (1,1)")
	execSource("CREATE VIEW " + sq(view) + " AS SELECT p.ID, NVL(p.NAME,'AbC') DISPLAY_NAME FROM " + sq(parent) + " p WHERE p.ID=1")
	execSource("CREATE TABLE " + sq(high) + " (ID NUMBER(18) PRIMARY KEY, TS TIMESTAMP(9))")
	execSource("INSERT INTO " + sq(high) + ` VALUES (1,TO_TIMESTAMP('2024-02-29 23:34:56.123456789','YYYY-MM-DD HH24:MI:SS.FF9'))`)
	execSource("CREATE TABLE " + sq(longTable) + " (ID NUMBER(18) PRIMARY KEY, BODY LONG)")
	execSource("INSERT INTO " + sq(longTable) + " VALUES (1,'LONG保留AbC中文')")
	execSource("CREATE TABLE " + sq(locatorTable) + " (ID NUMBER(18) PRIMARY KEY, RID ROWID, URID UROWID(4000))")
	execSource("INSERT INTO " + sq(locatorTable) + " SELECT 1,ROWID,ROWID FROM " + sq(parent) + " WHERE ID=1")
	execSource("CREATE TABLE " + sq(xmlTable) + " (ID NUMBER(18) PRIMARY KEY, DOC XMLTYPE)")
	execSource("INSERT INTO " + sq(xmlTable) + " VALUES (1,XMLTYPE('<root><name>AbC中文</name></root>'))")

	run := func(t *testing.T, content, filter string) MigrationReport {
		r, e := source.NewOracleForDameng(srcDSN, srcSchema, source.ConnPoolConfig{})
		require.NoError(t, e)
		defer r.Close()
		w, e := target.NewDaMeng(dstDSN, dstSchema, target.ConnPoolConfig{})
		require.NoError(t, e)
		defer w.Close()
		job := &Job{LogCh: make(chan string, 4096)}
		m := NewMigrator(r, w, job, Config{Mode: "include", Filter: filter, Content: content, TargetDBType: "dameng", TargetSchema: dstSchema, PageSize: 1, MaxParallel: 1})
		return m.Run(ctx)
	}

	verifyData := func(t *testing.T) {
		var date, stamp, amount, body, nbody string
		var blob []byte
		var dayInterval, monthInterval int
		err := dst.QueryRowContext(ctx, `SELECT TO_CHAR("AT_DATE",'YYYY-MM-DD HH24:MI:SS'),TO_CHAR("AT_TS",'YYYY-MM-DD HH24:MI:SS.FF6'),
		CAST("AMOUNT" AS VARCHAR(90)),"BODY","NBODY","PAYLOAD",
		CASE WHEN "DUR"=TO_DSINTERVAL('-001 02:03:04.123456') THEN 1 ELSE 0 END,
		CASE WHEN "MONTHS_VAL"=TO_YMINTERVAL('-001-02') THEN 1 ELSE 0 END
		FROM `+dq(parent)+` WHERE "ID"=1`).Scan(&date, &stamp, &amount, &body, &nbody, &blob, &dayInterval, &monthInterval)
		require.NoError(t, err)
		require.Equal(t, "2024-02-29 23:34:56", date, "Oracle DATE 包含时分秒")
		require.Equal(t, "2024-02-29 23:34:56.123456", stamp)
		require.Equal(t, "12345678901234567890.123456789012345678", amount)
		require.Equal(t, "保留AbC中文", body)
		require.Equal(t, "保留大小写AbC", nbody)
		require.Equal(t, []byte{0, 0xaa, 0xff}, blob)
		require.Equal(t, 1, dayInterval)
		require.Equal(t, 1, monthInterval)
		var count int
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(parent)).Scan(&count))
		require.Equal(t, 2, count)
		var emptyClob, emptyNClob, emptyBlob int
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT CASE WHEN "BODY" IS NULL THEN -1 ELSE LENGTH("BODY") END,CASE WHEN "NBODY" IS NULL THEN -1 ELSE LENGTH("NBODY") END,CASE WHEN "PAYLOAD" IS NULL THEN -1 ELSE LENGTH("PAYLOAD") END FROM `+dq(parent)+` WHERE "NAME"='sequence row'`).Scan(&emptyClob, &emptyNClob, &emptyBlob))
		require.Zero(t, emptyClob, "空 CLOB 不能变为 NULL")
		require.Zero(t, emptyNClob, "空 NCLOB 不能变为 NULL")
		require.Zero(t, emptyBlob, "空 BLOB 不能变为 NULL")
		var nname, vname, ltz string
		var doubleValue, sourceDouble float64
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT RTRIM("NNAME"),"VNAME",TO_CHAR("AT_LTZ" AT TIME ZONE '+00:00','YYYY-MM-DD HH24:MI:SS.FF6'),"DOUBLE_VAL" FROM `+dq(parent)+` WHERE "ID"=1`).Scan(&nname, &vname, &ltz, &doubleValue))
		require.Equal(t, "中文AbC", nname)
		require.Equal(t, "中文Mixed", vname)
		require.Equal(t, "2024-03-01 04:34:56.123456", ltz)
		require.NoError(t, src.QueryRowContext(ctx, "SELECT DOUBLE_VAL FROM "+sq(parent)+" WHERE ID=1").Scan(&sourceDouble))
		require.Equal(t, sourceDouble, doubleValue)
		var sourceFloat string
		require.NoError(t, src.QueryRowContext(ctx, "SELECT TO_CHAR(FLOAT_VAL,'TM9','NLS_NUMERIC_CHARACTERS=''.,''') FROM "+sq(parent)+" WHERE ID=1").Scan(&sourceFloat))
		var sameFloat, sameBinary int
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT CASE WHEN "FLOAT_VAL"=CAST(? AS FLOAT(126)) THEN 1 ELSE 0 END,CASE WHEN "BINARY_VAL"=CAST(16777216 AS FLOAT) THEN 1 ELSE 0 END FROM `+dq(parent)+` WHERE "ID"=1`, sourceFloat).Scan(&sameFloat, &sameBinary))
		require.Equal(t, 1, sameFloat, "FLOAT 按目标实际浮点族允许正常舍入")
		require.Equal(t, 1, sameBinary)
		var dateKind, units string
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT DATA_TYPE FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME='AT_DATE'", dstSchema, parent).Scan(&dateKind))
		if compatible == "2" {
			require.Equal(t, "DATE", strings.ToUpper(dateKind))
		} else {
			require.True(t, strings.HasPrefix(strings.ToUpper(dateKind), "TIMESTAMP") || strings.EqualFold(dateKind, "DATETIME"), dateKind)
		}
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT CHAR_USED FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME='VNAME'", dstSchema, parent).Scan(&units))
		require.Equal(t, "C", units, "NVARCHAR 的 DDL 必须显式使用字符长度单位")
	}

	filter := parent + "," + child
	t.Run("schema_and_data_only", func(t *testing.T) {
		report := run(t, "schema_only", filter)
		require.False(t, report.HasFailures(), "%+v", report)
		var count int
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq(parent)).Scan(&count))
		require.Zero(t, count)
		// The schema-only pass has already installed the FK. Populate its
		// referenced table first rather than depending on dictionary ordering.
		report = run(t, "data_only", parent)
		require.False(t, report.HasFailures(), "%+v", report)
		report = run(t, "data_only", child)
		require.False(t, report.HasFailures(), "%+v", report)
		verifyData(t)
	})
	cleanTarget(ctx)
	t.Run("both_objects_and_view", func(t *testing.T) {
		report := run(t, "both", filter)
		require.False(t, report.HasFailures(), "%+v", report)
		verifyData(t)
		var count int
		for _, query := range []struct {
			statement string
			table     string
		}{
			{"SELECT COUNT(*) FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=? AND CONSTRAINT_TYPE='P'", parent},
			{"SELECT COUNT(*) FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=? AND CONSTRAINT_TYPE='R'", child},
		} {
			require.NoError(t, dst.QueryRowContext(ctx, query.statement, dstSchema, query.table).Scan(&count))
			require.Equal(t, 1, count)
		}
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_INDEXES WHERE OWNER=? AND INDEX_NAME=?", dstSchema, index).Scan(&count))
		require.Equal(t, 1, count)
		var comment string
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COMMENTS FROM ALL_TAB_COMMENTS WHERE OWNER=? AND TABLE_NAME=?", dstSchema, parent).Scan(&comment))
		require.Equal(t, "Oracle到达梦测试AbC", comment)
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT COMMENTS FROM ALL_COL_COMMENTS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME='NAME'", dstSchema, parent).Scan(&comment))
		require.Equal(t, "列注释AbC", comment)
		// Include mode intentionally limits table work; exercise the separate
		// view-only entrypoint against the already migrated dependency table.
		reader, err := source.NewOracleForDameng(srcDSN, srcSchema, source.ConnPoolConfig{})
		require.NoError(t, err)
		defer reader.Close()
		writer, err := target.NewDaMeng(dstDSN, dstSchema, target.ConnPoolConfig{})
		require.NoError(t, err)
		defer writer.Close()
		vm := NewMigrator(reader, writer, &Job{LogCh: make(chan string, 1024)}, Config{TargetDBType: "dameng", TargetSchema: dstSchema})
		results := vm.MigrateViews(ctx, []string{view})
		require.Len(t, results, 1)
		require.Empty(t, results[0].Error)
		var displayName string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT "DISPLAY_NAME" FROM `+dq(view)).Scan(&displayName))
		require.Equal(t, "AbC中文", displayName)
		_, err = dst.ExecContext(ctx, "INSERT INTO "+dq(parent)+` ("NAME") VALUES ('target continuation')`)
		require.NoError(t, err)
		var continued int64
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT "ID" FROM `+dq(parent)+` WHERE "NAME"='target continuation'`).Scan(&continued))
		require.GreaterOrEqual(t, continued, int64(105), "不能低于源序列的安全后继值")
		require.Zero(t, (continued-100)%5)
	})
	cleanTarget(ctx)
	t.Run("long_and_rowid", func(t *testing.T) {
		report := run(t, "both", longTable+","+locatorTable)
		require.False(t, report.HasFailures(), "%+v", report)
		var body, rowid string
		var rid, urid []byte
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT "BODY" FROM `+dq(longTable)+` WHERE "ID"=1`).Scan(&body))
		require.Equal(t, "LONG保留AbC中文", body)
		require.NoError(t, src.QueryRowContext(ctx, "SELECT ROWIDTOCHAR(RID) FROM "+sq(locatorTable)+" WHERE ID=1").Scan(&rowid))
		require.Len(t, rowid, 18)
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT "RID","URID" FROM `+dq(locatorTable)+` WHERE "ID"=1`).Scan(&rid, &urid))
		require.Equal(t, []byte(rowid), rid)
		require.Equal(t, []byte(rowid), urid)
	})
	cleanTarget(ctx)
	t.Run("xmltype_capability", func(t *testing.T) {
		probe, probeErr := dst.QueryContext(ctx, "SELECT XMLSERIALIZE(CONTENT XMLTYPE('<root><name>AbC中文</name></root>') AS CLOB) FROM DUAL")
		if probeErr == nil {
			require.NoError(t, probe.Close())
		}
		report := run(t, "both", xmlTable)
		if probeErr != nil {
			require.True(t, report.HasFailures(), "缺少原生 XMLTYPE 时必须在建表前报告能力不足")
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?", dstSchema, xmlTable).Scan(&count))
			require.Zero(t, count)
			return
		}
		require.False(t, report.HasFailures(), "%+v", report)
		var serialized string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT XMLSERIALIZE(CONTENT "DOC" AS CLOB) FROM `+dq(xmlTable)+` WHERE "ID"=1`).Scan(&serialized))
		require.Contains(t, serialized, "<name>AbC中文</name>")
	})
	cleanTarget(ctx)
	t.Run("bfile_locator_without_file_reads", func(t *testing.T) {
		directory := os.Getenv("ORACLE_DM_TEST_BFILE_DIRECTORY")
		if directory == "" {
			t.Skip("BFILE 实库未验收：需配置已存在且有读取目录元数据权限的 ORACLE_DM_TEST_BFILE_DIRECTORY；测试不创建目录、不读取文件")
		}
		execSource("CREATE TABLE " + sq(bfileTable) + " (ID NUMBER(18) PRIMARY KEY, LOC BFILE)")
		_, err := src.ExecContext(ctx, "INSERT INTO "+sq(bfileTable)+" VALUES (1,BFILENAME(:1,:2))", directory, "dbgold_locator_only.txt")
		require.NoError(t, err)
		probe, probeErr := dst.QueryContext(ctx, "SELECT BFILENAME(?,?) FROM DUAL WHERE 1=0", nil, nil)
		if probeErr == nil {
			require.NoError(t, probe.Close())
		}
		report := run(t, "both", bfileTable)
		if probeErr != nil {
			require.True(t, report.HasFailures(), "目标缺少 BFILE 类型时不能自动降级")
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?", dstSchema, bfileTable).Scan(&count))
			require.Zero(t, count)
			return
		}
		require.False(t, report.HasFailures(), "%+v", report)
		var locator string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT "LOC" FROM `+dq(bfileTable)+` WHERE "ID"=1`).Scan(&locator))
		require.Equal(t, directory+":dbgold_locator_only.txt", locator)
	})
	cleanTarget(ctx)
	t.Run("native_spatial_point", func(t *testing.T) {
		if os.Getenv("ORACLE_DM_TEST_SPATIAL") != "1" {
			t.Skip("空间类型实库未验收：需显式配置 ORACLE_DM_TEST_SPATIAL=1 及已有 Oracle MDSYS、DM SYSGEO2/DMGEO2 组件；测试不安装组件")
		}
		execSource("CREATE TABLE " + sq(spatialTable) + " (ID NUMBER(18) PRIMARY KEY, GEOM MDSYS.SDO_GEOMETRY)")
		execSource("INSERT INTO " + sq(spatialTable) + " VALUES (1,MDSYS.SDO_GEOMETRY(2001,4326,MDSYS.SDO_POINT_TYPE(1.25,-2.5,NULL),NULL,NULL))")
		report := run(t, "both", spatialTable)
		require.False(t, report.HasFailures(), "%+v", report)
		var x, y float64
		var srid int
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT DMGEO2.ST_X("GEOM"),DMGEO2.ST_Y("GEOM"),DMGEO2.ST_SRID("GEOM") FROM `+dq(spatialTable)+` WHERE "ID"=1`).Scan(&x, &y, &srid))
		require.Equal(t, 1.25, x)
		require.Equal(t, -2.5, y)
		require.Equal(t, 4326, srid)
	})
	cleanTarget(ctx)
	t.Run("timestamp9_capability_gate", func(t *testing.T) {
		const expected = "2024-02-29 23:34:56.123456789"
		var observed string
		probeErr := dst.QueryRowContext(ctx, `SELECT TO_CHAR(CAST(TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF9') AS TIMESTAMP(9)), 'YYYY-MM-DD HH24:MI:SS.FF9') FROM DUAL`, expected).Scan(&observed)
		report := run(t, "both", high)
		if probeErr != nil || observed != expected {
			require.True(t, report.HasFailures(), "不能精确保留九位小数秒时必须在写入前拒绝 TIMESTAMP(9)")
			var count int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=? AND TABLE_NAME=?", dstSchema, high).Scan(&count))
			require.Zero(t, count)
			return
		}
		require.False(t, report.HasFailures(), "%+v", report)
		var stamp string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT TO_CHAR("TS",'YYYY-MM-DD HH24:MI:SS.FF9') FROM `+dq(high)).Scan(&stamp))
		require.Equal(t, expected, stamp)
	})
}
