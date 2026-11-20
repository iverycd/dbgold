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

func TestDamengNativeIntegrationEditFailedViewAndResumeCompile(t *testing.T) {
	dsn, schema := os.Getenv("DAMENG_DM_TEST_TARGET_DSN"), os.Getenv("DAMENG_DM_TEST_TARGET_SCHEMA")
	if os.Getenv("DAMENG_DM_TEST_ALLOW_DDL") != "1" || dsn == "" || schema == "" {
		t.Skip("需要单独的达梦测试模式、目标 DSN 和 DAMENG_DM_TEST_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	w, err := NewDaMengNative(dsn, schema, ConnPoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	name := fmt.Sprintf("RepairView%X", time.Now().UnixNano())
	qualified := damengnative.Qualified(w.schema, name)
	bad := "CREATE VIEW " + qualified + " AS SELECT MISSING_COLUMN FROM DUAL"
	compile := "ALTER VIEW " + qualified + " COMPILE"
	err = w.ExecuteDamengNative(ctx, bad)
	require.ErrorContains(t, err, "MISSING_COLUMN")
	original := damengnative.Object{
		ID: name, Kind: "view", Schema: w.schema, Name: name, CompileSQL: compile,
		Units: []damengnative.Unit{{SQL: bad, Phase: "after_data"}, {SQL: compile, Phase: "compile"}},
	}
	obj := repair.Object{
		ID: name, Kind: original.Kind, TargetName: name, Schema: w.schema, Status: "failed",
		Native: &original, NativeFingerprint: damengnative.Fingerprint(original),
		Units: []repair.Unit{{ID: "create", SQL: bad, OriginalSQL: bad, Status: "failed"}, {ID: "compile", SQL: compile, OriginalSQL: compile, Status: "pending"}},
	}
	sql := "CREATE OR REPLACE VIEW " + damengnative.Quote(name) + " AS SELECT 1 AS ID FROM DUAL UNION ALL SELECT 2 AS ID FROM DUAL"
	draft, err := obj.WithNativeViewDraft(sql, true)
	require.NoError(t, err)
	out, err := ExecuteRepair(ctx, w, draft, draft.Units, nil)
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = w.ExecuteDamengNative(clean, "DROP VIEW "+qualified)
	})
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[0].Status)
	require.Equal(t, "succeeded", out[1].Status)
	state, status, err := w.editedViewState(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "VALID", status)
	require.True(t, sameNativeViewDefinition(w.schema, name, sql, state))
	var total int
	require.NoError(t, w.db.QueryRowContext(ctx, "SELECT SUM(ID) FROM "+qualified).Scan(&total))
	require.Equal(t, 3, total)

	// Resume a persisted partial run against the definition actually installed.
	draft.Units[0] = out[0]
	draft.Units[1].Status = "failed"
	draft.Status = "failed"
	resumed, err := draft.WithNativeViewDraft(sql, true)
	require.NoError(t, err)
	require.Equal(t, "succeeded", resumed.Units[0].Status)
	out, err = ExecuteRepair(ctx, w, resumed, resumed.Units, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", out[1].Status)
}
