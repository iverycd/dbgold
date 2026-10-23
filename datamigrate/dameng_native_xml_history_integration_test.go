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

// XML's native flag must survive every storage declaration, including the
// immutable history stage, exchange buffer, existing interval children, and
// the temporary empty-base epoch. XMLTYPE constructors are used only in fixture
// writes; migration copies the source's already stored representation.
func TestDamengNativeXMLTypeMaterializedHistoryIntegration(t *testing.T) {
	sd, ss, dd, ds := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA"), os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || os.Getenv("DAMENG_NATIVE_ALLOW_MV_DDL") != "1" || sd == "" || ss == "" || dd == "" || ds == "" {
		t.Skip("需要预建空DM8专用Schema、SOURCE/TARGET_DSN/SCHEMA及两个ALLOW_DDL标志")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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
		db     *sql.DB
		schema string
	}{{src, ss}, {dst, ds}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&count))
		require.Zero(t, count)
	}
	for _, tc := range []struct {
		name, basePartition, storePartition string
		id                                  int
	}{
		{"ordinary", "", "", 1},
		{"prebuilt_range", "", ` PARTITION BY RANGE("Id")(PARTITION "Low" VALUES LESS THAN(10),PARTITION "High" VALUES LESS THAN(MAXVALUE))`, 15},
		{"interval_base", ` PARTITION BY RANGE("Id") INTERVAL(10)(PARTITION "Initial" VALUES LESS THAN(0))`, ` PARTITION BY RANGE("Id")(PARTITION "Low" VALUES LESS THAN(10),PARTITION "High" VALUES LESS THAN(MAXVALUE))`, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sq := func(name string) string { return damengnative.Qualified(ss, name) }
			dq := func(name string) string { return damengnative.Qualified(ds, name) }
			exec := func(db *sql.DB, text string, args ...any) {
				_, err := db.ExecContext(ctx, text, args...)
				require.NoError(t, err, text)
			}
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
				_, _ = src.ExecContext(clean, "DROP MATERIALIZED VIEW "+sq("XMLSnapshot"))
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq("XMLStore")+" CASCADE CONSTRAINTS")
				_, _ = src.ExecContext(clean, "DROP TABLE "+sq("XMLBase")+" CASCADE CONSTRAINTS")
			}()
			columns := `("Id" INT,"Value" INT,"Document" XMLTYPE)`
			exec(src, "CREATE TABLE "+sq("XMLBase")+columns+tc.basePartition)
			exec(src, "CREATE TABLE "+sq("XMLStore")+columns+tc.storePartition)
			xml := `<?xml version="1.0"?><root a="a&amp;b"><![CDATA[中文😀 < &]]><!--retain--><empty/>  </root>`
			exec(src, "INSERT INTO "+sq("XMLBase")+` VALUES(?,10,XMLTYPE(?))`, tc.id, xml)
			exec(src, "CREATE MATERIALIZED VIEW "+sq("XMLSnapshot")+" FOR "+sq("XMLStore")+` ON PREBUILT TABLE REFRESH FORCE ON DEMAND AS SELECT "Id","Value","Document" FROM `+sq("XMLBase")+` UNION ALL SELECT "Id","Value","Document" FROM `+sq("XMLBase"))
			exec(src, "REFRESH MATERIALIZED VIEW "+sq("XMLSnapshot")+" COMPLETE")
			var bound string
			require.NoError(t, src.QueryRowContext(ctx, `SELECT t.NAME FROM SYS.SYSOBJECTS v JOIN SYS.SYSOBJECTS t ON t.ID=BITAND(v.INFO3,4294967295) JOIN SYS.SYSOBJECTS s ON s.ID=v.SCHID WHERE s.NAME=? AND v.NAME='XMLSnapshot'`, ss).Scan(&bound))
			require.Equal(t, "XMLStore", bound)
			var sourcePartitions int
			require.NoError(t, src.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME=?`, ss, bound).Scan(&sourcePartitions))
			if tc.storePartition != "" {
				require.Equal(t, 2, sourcePartitions, "范围分区必须属于物化视图实际绑定的预建存储")
			} else {
				require.Zero(t, sourcePartitions)
			}
			exec(src, "DELETE FROM "+sq("XMLBase"))
			r, err := source.NewDaMengNative(sd, ss, source.ConnPoolConfig{})
			require.NoError(t, err)
			defer r.Close()
			w, err := target.NewDaMengNative(dd, ds, target.ConnPoolConfig{})
			require.NoError(t, err)
			defer w.Close()
			var saved RepairManifest
			m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: ds, CaptureRepair: true, ValidateData: true, RepairSink: func(plan RepairManifest) error { saved = plan; return nil }})
			frozen = m.PreflightDamengNative(ctx)
			require.True(t, frozen.Ready, "%+v", frozen.Issues)
			report := m.Run(ctx)
			if report.HasFailures() {
				b, _ := json.Marshal(report)
				t.Logf("XML history report: %s;logs=%v", b, drainLogs(m.job))
			}
			require.False(t, report.HasFailures())
			require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
			encoded, err := json.Marshal(saved)
			require.NoError(t, err)
			var reload RepairManifest
			require.NoError(t, json.Unmarshal(encoded, &reload))
			require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reload.Objects))
			var targetPartitions int
			require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_PARTITIONS WHERE TABLE_OWNER=? AND TABLE_NAME='XMLStore'`, ds).Scan(&targetPartitions))
			require.Equal(t, sourcePartitions, targetPartitions)
			var sourceXML, targetXML string
			require.NoError(t, src.QueryRowContext(ctx, "SELECT \"Document\" FROM "+sq("XMLSnapshot")+" WHERE ROWNUM=1").Scan(&sourceXML))
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Document\" FROM "+dq("XMLSnapshot")+" WHERE ROWNUM=1").Scan(&targetXML))
			require.Equal(t, sourceXML, targetXML)
			rows, err := dst.QueryContext(ctx, `SELECT c.TABLE_NAME,c.DATA_SCALE FROM ALL_TAB_COLUMNS c JOIN ALL_TABLES t ON t.OWNER=c.OWNER AND t.TABLE_NAME=c.TABLE_NAME WHERE c.OWNER=? AND c.COLUMN_NAME='Document' ORDER BY c.TABLE_NAME`, ds)
			require.NoError(t, err)
			count := 0
			for rows.Next() {
				var table string
				var scale int
				require.NoError(t, rows.Scan(&table, &scale))
				require.Equal(t, 8, scale, table+" XMLTYPE属性不能降为普通TEXT")
				count++
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Positive(t, count)
			exec(dst, "INSERT INTO "+dq("XMLBase")+` VALUES(5,20,XMLTYPE('<new/>'))`)
			exec(dst, "REFRESH MATERIALIZED VIEW "+dq("XMLSnapshot")+" FORCE")
			var n int
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dq("XMLSnapshot")+` WHERE "Value"=20`).Scan(&n))
			require.Equal(t, 2, n)
		})
	}
}
