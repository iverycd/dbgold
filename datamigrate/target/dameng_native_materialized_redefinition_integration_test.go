package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestDamengNativeHashRedefinitionIntegration(t *testing.T) {
	dsn, owner := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || owner == "" {
		t.Skip("HASH 原生重定义测试需要专用测试模式及 DAMENG_DM_TEST_ALLOW_DDL=1")
	}
	w, err := NewDaMengNative(dsn, owner, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := w.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	prefix := fmt.Sprintf("HashR%X", time.Now().UnixNano())
	base, store, mv := prefix+"B", prefix+"S", prefix+"M"
	stage, work, swap := prefix+"I", prefix+"W", prefix+"T"
	log, buffer := "MLOG$_"+base, prefix+"L"
	q := func(n string) string { return damengnative.Qualified(owner, n) }
	exec := func(s string) { _, e := conn.ExecContext(ctx, s); require.NoError(t, e, s) }
	v := damengnative.MaterializedHistoryView{Name: mv, Table: store, Staging: stage, Redefinition: &damengnative.MaterializedRedefinitionPlan{WorkingTable: work, SwapName: swap}}
	require.NoError(t, w.validateHistoryRedefinition(ctx, []damengnative.MaterializedHistoryView{v}))
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = conn.ExecContext(clean, `CALL SYS.DBMS_REDEFINITION.ABORT_REDEF_TABLE(?,?,?)`, owner, work, store)
		_, _ = conn.ExecContext(clean, "DROP MATERIALIZED VIEW "+q(mv))
		_, _ = conn.ExecContext(clean, "DROP MATERIALIZED VIEW LOG ON "+q(base))
		for _, name := range []string{swap, work, store, stage, buffer, base} {
			_, _ = conn.ExecContext(clean, "DROP TABLE "+q(name))
		}
	}()
	exec("SET SCHEMA " + damengnative.Quote(owner))
	exec("CREATE TABLE " + q(base) + ` ("ID" INT,"V" INT)`)
	exec("INSERT INTO " + q(base) + " VALUES(1,10)")
	exec("INSERT INTO " + q(base) + " VALUES(1,10)")
	exec("INSERT INTO " + q(base) + " VALUES(2,20)")
	exec("CREATE MATERIALIZED VIEW LOG ON " + q(base) + ` WITH ROWID("ID","V")`)
	exec("CREATE TABLE " + q(store) + ` ("ID" INT,"V" INT,"RID" ROWID NOT NULL) PARTITION BY HASH("ID") (PARTITION "H1",PARTITION "H2")`)
	exec("CREATE MATERIALIZED VIEW " + q(mv) + " FOR " + q(store) + ` ON PREBUILT TABLE REFRESH FAST WITH ROWID ON DEMAND AS SELECT "ID","V",ROWID AS "RID" FROM ` + q(base))
	exec("CREATE INDEX " + q(prefix+"X") + " ON " + q(mv) + `("V")`)
	exec("REFRESH MATERIALIZED VIEW " + q(mv) + " COMPLETE")
	exec("CREATE TABLE " + q(stage) + " AS SELECT * FROM " + q(mv))
	exec("CREATE TABLE " + q(work) + ` ("ID" INT,"V" INT,"RID" ROWID NOT NULL)`)
	before, err := historyReadRedefinitionState(ctx, conn, owner, v)
	require.NoError(t, err)
	var originalDDL, originalViewDDL string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, store, owner).Scan(&originalDDL))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('MATERIALIZED_VIEW',?,?) FROM DUAL`, mv, owner).Scan(&originalViewDDL))
	// Save real native changes before the fresh target baseline purges them.
	exec("UPDATE " + q(base) + ` SET "V"=11 WHERE "ID"=1`)
	exec("DELETE FROM " + q(base) + ` WHERE "ID"=2`)
	exec("INSERT INTO " + q(base) + " VALUES(3,30)")
	exec("CREATE TABLE " + q(buffer) + ` ("M_ROW$$" VARCHAR(128) NOT NULL,"ID" INT,"V" INT,"SNAPTIME$$" DATETIME(6) NOT NULL,"DMLTYPE$$" CHAR(1) NOT NULL,"SEQUENCE$$" BIGINT IDENTITY(1,1) NOT NULL) PARTITION BY RANGE("SEQUENCE$$") (PARTITION "P" VALUES LESS THAN(MAXVALUE))`)
	exec("CREATE INDEX " + q(prefix+"LS") + " ON " + q(buffer) + `("SNAPTIME$$")`)
	exec("CREATE INDEX " + q(prefix+"LQ") + " ON " + q(buffer) + `("SEQUENCE$$")`)
	exec("SET IDENTITY_INSERT " + q(buffer) + " ON")
	exec("INSERT INTO " + q(buffer) + `("M_ROW$$","ID","V","SNAPTIME$$","DMLTYPE$$","SEQUENCE$$") SELECT "M_ROW$$","ID","V",TIMESTAMP '4000-01-01 00:00:00',"DMLTYPE$$",ROW_NUMBER() OVER(ORDER BY "SEQUENCE$$")-COUNT(*) OVER()-1 FROM ` + q(log))
	exec("SET IDENTITY_INSERT " + q(buffer) + " OFF")
	// Simulate lost START and every FINISH/RENAME response. Each repair derives
	// the physical binding from the catalog; no successful name swap is replayed.
	for phase := -1; phase < 4; phase++ {
		require.NoError(t, historyReconcileRedefinition(ctx, conn, owner, v))
		exec("REFRESH MATERIALIZED VIEW " + q(mv) + " COMPLETE")
		exec("TRUNCATE TABLE " + q(work))
		exec("INSERT INTO " + q(work) + " SELECT * FROM " + q(stage))
		_, err = conn.ExecContext(ctx, `CALL SYS.DBMS_REDEFINITION.START_REDEF_TABLE(?,?,?,NULL,2)`, owner, work, store)
		require.NoError(t, err)
		if phase == -1 {
			require.NoError(t, historyReconcileRedefinition(ctx, conn, owner, v))
			continue
		}
		_, err = conn.ExecContext(ctx, `CALL SYS.DBMS_REDEFINITION.FINISH_REDEF_TABLE(?,?,?)`, owner, work, store)
		require.NoError(t, err)
		if phase >= 1 {
			exec("ALTER TABLE " + q(store) + " RENAME TO " + damengnative.Quote(swap))
		}
		if phase >= 2 {
			exec("ALTER TABLE " + q(work) + " RENAME TO " + damengnative.Quote(store))
		}
		if phase >= 3 {
			exec("ALTER TABLE " + q(swap) + " RENAME TO " + damengnative.Quote(work))
		}
		require.NoError(t, historyReconcileRedefinition(ctx, conn, owner, v))
		after, e := historyReadRedefinitionState(ctx, conn, owner, v)
		require.NoError(t, e)
		require.Equal(t, before.boundID, after.boundID)
		var currentDDL, viewDDL string
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?) FROM DUAL`, store, owner).Scan(&currentDDL))
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('MATERIALIZED_VIEW',?,?) FROM DUAL`, mv, owner).Scan(&viewDDL))
		require.Equal(t, originalDDL, currentDDL)
		require.Equal(t, originalViewDDL, viewDDL)
	}
	// Exercise the production entry point as well as interrupted calls.
	exec("REFRESH MATERIALIZED VIEW " + q(mv) + " COMPLETE")
	exec("TRUNCATE TABLE " + q(work))
	exec("INSERT INTO " + q(work) + " SELECT * FROM " + q(stage))
	require.NoError(t, historyRebaseStorage(ctx, conn, owner, v))
	var oldRows, indexes int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q(mv)+` WHERE "ID"=1 AND "V"=10`).Scan(&oldRows))
	require.Equal(t, 2, oldRows)
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_INDEXES WHERE OWNER=? AND INDEX_NAME=? AND TABLE_NAME=?`, owner, prefix+"X", store).Scan(&indexes))
	require.Equal(t, 1, indexes)
	exec("ALTER TABLE " + q(buffer) + ` EXCHANGE PARTITION "P" WITH TABLE ` + q(log))
	exec("UPDATE " + q(base) + ` SET "V"=13 WHERE "ID"=1`)
	exec("DELETE FROM " + q(base) + ` WHERE "ID"=3`)
	exec("INSERT INTO " + q(base) + " VALUES(4,40)")
	exec("REFRESH MATERIALIZED VIEW " + q(mv) + " FAST")
	var mismatch int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM ((SELECT "ID","V",ROWIDTOCHAR("RID") AS R FROM `+q(mv)+` MINUS SELECT "ID","V",ROWIDTOCHAR(ROWID) FROM `+q(base)+`) UNION ALL (SELECT "ID","V",ROWIDTOCHAR(ROWID) AS R FROM `+q(base)+` MINUS SELECT "ID","V",ROWIDTOCHAR("RID") FROM `+q(mv)+`))`).Scan(&mismatch))
	require.Zero(t, mismatch)
	var rows int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q(mv)).Scan(&rows))
	require.Equal(t, 3, rows)
}
