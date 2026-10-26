package target

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

// This test changes only a user-provided, explicitly authorized empty DM schema.
// Servers that retain INVALID trigger objects exercise the owned OR REPLACE
// path; servers that reject CREATE atomically exercise absent-object retry.
func TestManualTableTriggerRepairIntegration(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("需要独立空达梦测试模式、DAMENG_DM_TEST_TARGET_DSN / TARGET_SCHEMA 和 DAMENG_DM_TEST_ALLOW_DDL=1")
	}
	w, err := NewDaMeng(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var count int
	require.NoError(t, w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=?`, schema).Scan(&count))
	require.Zero(t, count, "只允许在最初为空的测试 schema 执行")
	name := fmt.Sprintf("NMAN%X", time.Now().UnixNano())
	table, log := name+"T", name+"L"
	qualified := func(value string) string { return damengnative.Qualified(schema, value) }
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = w.db.ExecContext(cleanup, "DROP TRIGGER "+qualified(name))
		_, _ = w.db.ExecContext(cleanup, "DROP TABLE "+qualified(table))
		_, _ = w.db.ExecContext(cleanup, "DROP TABLE "+qualified(log))
	})
	require.NoError(t, manualIntegrationExec(ctx, w, "CREATE TABLE "+qualified(table)+`("ID" INT)`))
	require.NoError(t, manualIntegrationExec(ctx, w, "CREATE TABLE "+qualified(log)+`("ID" INT)`))
	bad := "CREATE OR REPLACE TRIGGER " + qualified(name) + " AFTER INSERT ON " + qualified(table) + " FOR EACH ROW BEGIN INSERT INTO " + qualified(log) + `("MISSING_COLUMN") VALUES(:NEW."ID"); END;`
	good := "CREATE OR REPLACE TRIGGER " + qualified(name) + " AFTER INSERT ON " + qualified(table) + " FOR EACH ROW BEGIN INSERT INTO " + qualified(log) + `("ID") VALUES(:NEW."ID"); END;`
	original := repair.Object{ID: name, Kind: "trigger", Name: name, TargetName: name, Schema: schema, Table: table, DDL: bad, Status: "failed"}
	o := original
	o.ManualTrigger = &repair.ManualTriggerPlan{SQL: bad, Revision: 1, Original: &original}
	o.Units = []repair.Unit{{ID: "create", SQL: bad, OriginalSQL: bad, Selected: true, Status: "pending"}}
	units, err := ExecuteRepair(ctx, w, o, o.Units, nil)
	require.Error(t, err, "引用不存在列的触发器必须创建或编译失败")
	require.Equal(t, "failed", units[0].Status)
	failed, readErr := readManualTriggerCatalog(ctx, w.db, schema, name)
	require.NoError(t, readErr)
	if failed.exists {
		require.Equal(t, "INVALID", failed.compile, "保留下来的无效对象必须走归属核验后的替换路径")
	} else {
		t.Log("服务端未保留无效对象，将验证失败后的重新创建")
	}
	o.ManualTrigger = &repair.ManualTriggerPlan{SQL: good, Revision: 2, Original: &original, PreviousSQL: []string{bad}}
	o.Units = []repair.Unit{{ID: "create-2", SQL: good, OriginalSQL: good, Selected: true, Status: "pending"}}
	units, err = ExecuteRepair(ctx, w, o, o.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", units[0].Status)
	require.NoError(t, manualIntegrationExec(ctx, w, "INSERT INTO "+qualified(table)+`("ID") VALUES(7)`))
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qualified(log)+` WHERE "ID"=7`).Scan(&count))
	require.Equal(t, 1, count, "修改后的表触发器必须实际执行正文 DML")
}

func manualIntegrationExec(ctx context.Context, w *DaMengWriter, query string) error {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer discardNativeEventConnection(conn)
	if _, err = conn.ExecContext(ctx, "SET SCHEMA "+damengnative.Quote(w.schema)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, query)
	return err
}
