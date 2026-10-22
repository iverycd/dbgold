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

// This acceptance suite is deliberately opt-in twice: it creates only uniquely
// named fixtures in a dedicated empty test schema and never changes INI values.
// It proves the DM engine can FAST-refresh after a copied prebuilt baseline;
// mocks alone cannot establish that engine behavior for a specific DM8 build.
func TestDamengNativeMaterializedBaselineIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || os.Getenv("DAMENG_DM_TEST_ALLOW_MV_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("物化视图实库未验收：需独立空测试模式及 DAMENG_DM_TEST_ALLOW_DDL=1、DAMENG_DM_TEST_ALLOW_MV_DDL=1、TARGET_DSN/TARGET_SCHEMA")
	}
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var initial int
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=?`, w.schema).Scan(&initial))
	require.Zero(t, initial, "测试模式最初必须为空")
	for n, variant := range []struct{ method, rule string }{{"FAST", "PRIMARY KEY"}, {"FORCE", "PRIMARY KEY"}, {"FAST", "ROWID"}, {"FORCE", "ROWID"}} {
		t.Run(variant.method+"_"+variant.rule, func(t *testing.T) {
			prefix := fmt.Sprintf("DNMV%X%d", time.Now().UnixNano(), n)
			base, old, store, mv := prefix+"B", prefix+"O", prefix+"S", prefix+"M"
			q := func(name string) string { return damengnative.Qualified(w.schema, name) }
			defer func() {
				clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				for _, ddl := range []string{"DROP MATERIALIZED VIEW " + q(mv), "DROP TABLE " + q(store) + " CASCADE CONSTRAINTS", "DROP TABLE " + q(base) + " CASCADE CONSTRAINTS", "DROP TABLE " + q(old) + " CASCADE CONSTRAINTS"} {
					_, _ = w.db.ExecContext(clean, ddl)
				}
			}()
			for _, table := range []string{base, old} {
				require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE TABLE "+q(table)+` ("Id" INT PRIMARY KEY,"Value" INT)`))
				if table == old && variant.rule == "ROWID" {
					_, err = w.db.ExecContext(ctx, "INSERT INTO "+q(table)+` VALUES(-1,-1)`)
					require.NoError(t, err)
					_, err = w.db.ExecContext(ctx, "DELETE FROM "+q(table)+` WHERE "Id"=-1`)
					require.NoError(t, err)
				}
				_, err = w.db.ExecContext(ctx, "INSERT INTO "+q(table)+` VALUES(1,10)`)
				require.NoError(t, err)
				_, err = w.db.ExecContext(ctx, "INSERT INTO "+q(table)+` VALUES(2,10)`)
				require.NoError(t, err)
			}
			require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE MATERIALIZED VIEW LOG ON "+q(base)+" WITH "+variant.rule))
			columns := []string{"Id", "Value"}
			sourceSelect := `SELECT "Id","Value"`
			query := sourceSelect + " FROM " + q(base)
			var rowids []int
			if variant.rule == "ROWID" {
				columns = append(columns, "Rid")
				rowids = []int{2}
				sourceSelect += `,ROWID AS "Rid"`
				query = sourceSelect + " FROM " + q(base)
			}
			// The old table has different physical ROWIDs but identical ordinary
			// values, exactly as a snapshot copied from another database would.
			require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE TABLE "+q(store)+" AS "+sourceSelect+" FROM "+q(old)))
			require.NoError(t, w.ExecuteDamengNative(ctx, "CREATE MATERIALIZED VIEW "+q(mv)+" FOR "+q(store)+" ON PREBUILT TABLE NEVER REFRESH AS "+query))
			o := damengnative.Object{ID: "fixture", Kind: "materialized_view", Name: mv, Schema: w.schema, Materialized: &damengnative.MaterializedState{Query: query, RefreshMethod: variant.method, RefreshRule: variant.rule, Staleness: "PROVEN_CURRENT", Baseline: true, SnapshotColumns: columns, RowIDColumns: rowids}, Units: []damengnative.Unit{{SQL: damengnative.MaterializedBaselineSQL(w.schema, mv), Phase: "mv_rebase"}}}
			require.NoError(t, w.ExecuteDamengNative(ctx, "ALTER MATERIALIZED VIEW "+q(mv)+" REFRESH COMPLETE ON DEMAND"))
			require.NoError(t, w.RebaseDamengNativeMaterialized(ctx, o))
			require.NoError(t, w.ExecuteDamengNative(ctx, "ALTER MATERIALIZED VIEW "+q(mv)+" REFRESH "+variant.method+" WITH "+variant.rule+" ON DEMAND"))
			_, err = w.db.ExecContext(ctx, "UPDATE "+q(base)+` SET "Value"=99 WHERE "Id"=1`)
			require.NoError(t, err)
			_, err = w.db.ExecContext(ctx, "INSERT INTO "+q(base)+` VALUES(3,30)`)
			require.NoError(t, err)
			_, err = w.db.ExecContext(ctx, "REFRESH MATERIALIZED VIEW "+q(mv)+" FAST")
			require.NoError(t, err, "必须证明原生增量日志基线有效，不能以 FORCE 回落 COMPLETE 掩盖失败")
			var value, count int
			require.NoError(t, w.db.QueryRowContext(ctx, `SELECT "Value" FROM `+q(mv)+` WHERE "Id"=1`).Scan(&value))
			require.Equal(t, 99, value)
			require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q(mv)).Scan(&count))
			require.Equal(t, 3, count)
			if len(rowids) > 0 {
				var mismatches int
				require.NoError(t, w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+q(mv)+` M JOIN `+q(base)+` B ON M."Id"=B."Id" WHERE M."Rid"<>B.ROWID`).Scan(&mismatches))
				require.Zero(t, mismatches)
			}
		})
	}
}
