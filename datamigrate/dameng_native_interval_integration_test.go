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

func TestDamengNativeIntervalNonemptyPreparationIntegration(t *testing.T) {
	sd, ss, dd, ds := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA"), os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || sd == "" || ss == "" || dd == "" || ds == "" {
		t.Skip("需要预建专用空DM8 Schema及DDL标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, e := sql.Open("dm", sd)
	require.NoError(t, e)
	defer src.Close()
	dst, e := sql.Open("dm", dd)
	require.NoError(t, e)
	defer dst.Close()
	ss, e = damengnative.ResolveSchema(ctx, src, ss)
	require.NoError(t, e)
	ds, e = damengnative.ResolveSchema(ctx, dst, ds)
	require.NoError(t, e)
	for _, side := range []struct {
		db *sql.DB
		s  string
	}{{src, ss}, {dst, ds}} {
		var n int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.s).Scan(&n))
		require.Zero(t, n, "需要初始为空的专用测试Schema")
	}
	sq := func(n string) string { return damengnative.Qualified(ss, n) }
	dq := func(n string) string { return damengnative.Qualified(ds, n) }
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
		for _, q := range []string{"DROP TABLE " + sq("IntervalChild") + " CASCADE CONSTRAINTS", "DROP TABLE " + sq("IntervalData") + " CASCADE CONSTRAINTS", "DROP TABLE " + sq("IntervalParent") + " CASCADE CONSTRAINTS", "DROP SEQUENCE " + sq("IntervalSequence")} {
			_, _ = src.ExecContext(clean, q)
		}
	}()
	exec(src, "CREATE SEQUENCE "+sq("IntervalSequence")+" START WITH 1 INCREMENT BY 1 NOCACHE")
	var first int64
	require.NoError(t, src.QueryRowContext(ctx, "SELECT "+sq("IntervalSequence")+".NEXTVAL FROM DUAL").Scan(&first))
	require.EqualValues(t, 1, first)
	exec(src, "CREATE TABLE "+sq("IntervalParent")+`("Id" INT PRIMARY KEY)`)
	exec(src, "INSERT INTO "+sq("IntervalParent")+" VALUES(15)")
	exec(src, "INSERT INTO "+sq("IntervalParent")+" VALUES(0)")
	exec(src, "CREATE TABLE "+sq("IntervalData")+`("Id" BIGINT IDENTITY(10,3),"A" INT NOT NULL,"V" INT DEFAULT ON NULL `+sq("IntervalSequence")+`.NEXTVAL NOT NULL,"ParentId" INT) PARTITION BY RANGE("A") INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(-5))`)
	exec(src, "CREATE INDEX "+sq("IntervalKeyIndex")+" ON "+sq("IntervalData")+`("Id") GLOBAL`)
	exec(src, "ALTER TABLE "+sq("IntervalData")+` ADD CONSTRAINT "IntervalPK" PRIMARY KEY("Id") USING INDEX `+sq("IntervalKeyIndex"))
	exec(src, "INSERT INTO "+sq("IntervalData")+`("A","V","ParentId") VALUES(15,10,15)`)
	exec(src, "INSERT INTO "+sq("IntervalData")+`("A","V","ParentId") VALUES(75,20,75)`)
	exec(src, "DELETE FROM "+sq("IntervalData")+` WHERE "A"=75`)
	exec(src, "ALTER TABLE "+sq("IntervalData")+` ADD CONSTRAINT "IntervalCheck" CHECK("V">0) ENABLE NOVALIDATE`)
	exec(src, "ALTER TABLE "+sq("IntervalData")+` ADD CONSTRAINT "IntervalFK" FOREIGN KEY("ParentId") REFERENCES `+sq("IntervalParent")+`("Id") ENABLE NOVALIDATE`)
	exec(src, "CREATE TABLE "+sq("IntervalChild")+`("ParentId" BIGINT REFERENCES `+sq("IntervalData")+`("Id"))`)
	exec(src, "INSERT INTO "+sq("IntervalChild")+" VALUES(10)")
	var emptyAuto string
	require.NoError(t, src.QueryRowContext(ctx, `SELECT PARTITION_NAME FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME='IntervalData' AND HIGH_VALUE='85'`, ss).Scan(&emptyAuto))
	exec(src, "ALTER TABLE "+sq("IntervalData")+" RENAME PARTITION "+damengnative.Quote(emptyAuto)+` TO "EmptyArchive"`)
	r, e := source.NewDaMengNative(sd, ss, source.ConnPoolConfig{})
	require.NoError(t, e)
	defer r.Close()
	w, e := target.NewDaMengNative(dd, ds, target.ConnPoolConfig{})
	require.NoError(t, e)
	defer w.Close()
	var manifest RepairManifest
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: ds, CaptureRepair: true, ValidateData: true, RepairSink: func(p RepairManifest) error { manifest = p; return nil }})
	frozen = m.PreflightDamengNative(ctx)
	require.True(t, frozen.Ready, "%+v", frozen.Issues)
	report := m.Run(ctx)
	if report.HasFailures() {
		b, _ := json.Marshal(report)
		t.Logf("interval nonempty report: %s;logs=%v", b, drainLogs(m.job))
		for _, side := range []struct {
			db *sql.DB
			s  string
		}{{src, ss}, {dst, ds}} {
			for _, name := range []string{"IntervalData", "IntervalSequence"} {
				kind := "TABLE"
				if name == "IntervalSequence" {
					kind = "SEQUENCE"
				}
				var ddl string
				err := side.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(?,?,?) FROM DUAL`, kind, name, side.s).Scan(&ddl)
				t.Logf("diagnostic %s %s ddl=%s err=%v", side.s, name, ddl, err)
			}
			var child string
			err := side.db.QueryRowContext(ctx, `SELECT p.NAME FROM SYS.SYSOBJECTS p JOIN SYS.SYSHPARTTABLEINFO h ON h.PART_TABLE_ID=p.ID JOIN SYS.SYSOBJECTS t ON t.ID=h.BASE_TABLE_ID JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID WHERE s.NAME=? AND t.NAME='IntervalData' AND h.PARTITION_NAME='Initial'`, side.s).Scan(&child)
			if err == nil {
				var ddl string
				err = side.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, child, side.s).Scan(&ddl)
				t.Logf("diagnostic %s physical initial ddl=%s err=%v", side.s, ddl, err)
			} else {
				t.Logf("diagnostic %s initial lookup=%v", side.s, err)
			}
			var last int64
			err = side.db.QueryRowContext(ctx, `SELECT LAST_NUMBER FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? AND SEQUENCE_NAME='IntervalSequence'`, side.s).Scan(&last)
			t.Logf("diagnostic %s sequence last=%d err=%v", side.s, last, err)
		}
	}
	require.False(t, report.HasFailures())
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	b, e := json.Marshal(manifest)
	require.NoError(t, e)
	var reload RepairManifest
	require.NoError(t, json.Unmarshal(b, &reload))
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects))
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME='IntervalData' AND PARTITION_NAME='EmptyArchive' AND HIGH_VALUE='85'`, ds).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("IntervalData")).Scan(&count))
	require.Equal(t, 1, count)
	// The migration and its frozen-source verification are complete. Exercise
	// fresh writes on both sides only now, using the real source continuation
	// as well as the frozen active-sequence boundary.
	exec(src, "INSERT INTO "+sq("IntervalData")+`("A","V","ParentId") VALUES(0,NULL,0)`)
	var sourceID, sourceV int64
	require.NoError(t, src.QueryRowContext(ctx, "SELECT \"Id\",\"V\" FROM "+sq("IntervalData")+` WHERE "A"=0`).Scan(&sourceID, &sourceV))
	exec(dst, "INSERT INTO "+dq("IntervalData")+`("A","V","ParentId") VALUES(0,NULL,0)`)
	var id, v int64
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Id\",\"V\" FROM "+dq("IntervalData")+` WHERE "A"=0`).Scan(&id, &v))
	require.EqualValues(t, 13, id, "准备分区不能消费自增号")
	require.GreaterOrEqual(t, id, sourceID, "安全续号不得退到源未来实际分配值之前")
	require.EqualValues(t, 2, v, "准备分区不能执行ON NULL默认序列")
	require.Equal(t, sourceV, v, "源与目标未来显式NULL默认序列分配应相同")
}
