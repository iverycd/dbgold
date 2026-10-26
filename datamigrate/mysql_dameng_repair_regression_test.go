package datamigrate

import (
	"context"
	"slices"
	"testing"

	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengFailedTriggerKeepsTrustedPlanWhenRead(t *testing.T) {
	m, _, w := mysqlDMTriggerFixture(t)
	m.cfg.CaptureRepair = true
	w.failName = "FILLAMOUNT"
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Triggers.Failed)
	objects := slices.Clone(report.RepairManifest.Objects)
	EnrichRepairReferences(objects, report)
	found := false
	for _, object := range objects {
		if object.MySQLDamengTrigger == nil {
			continue
		}
		found = true
		require.Equal(t, "failed", object.Status)
		require.False(t, object.DiagnosticOnly, "legacy report evidence must not revoke an authenticated plan")
		require.True(t, object.CanExecuteDDL())
		require.Len(t, object.Units, 1)
		require.Equal(t, object.MySQLDamengTrigger.TriggerSQL, object.Units[0].SQL)
	}
	require.True(t, found)
}

func TestMySQLDamengTriggerRecoveryKeepsFrozenMapping(t *testing.T) {
	m, _, w := mysqlDMTriggerFixture(t)
	m.cfg.Content, m.cfg.CaptureRepair = "schema_only", true
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Triggers.Success)
	var table, trigger repair.Object
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "table" && object.SourceTable == "OrderData" {
			table = object
		}
		if object.MySQLDamengTrigger != nil {
			trigger = object
		}
	}
	require.NotNil(t, trigger.MySQLDamengTrigger)
	frozen := repair.Hash(trigger.MySQLDamengTrigger)
	units := repair.Hash(trigger.Units)
	unchanged := regenerateRepairCandidate(w, m.cfg, table, trigger, map[string]string{"Id": "ID", "Amount": "AMOUNT"})
	require.False(t, unchanged.RequiresReview)
	require.False(t, unchanged.DiagnosticOnly)
	require.Equal(t, frozen, repair.Hash(unchanged.MySQLDamengTrigger))
	for _, mapping := range []map[string]string{
		{"Id": "ID", "Amount": "OTHER"},
		{"Id": "AMOUNT", "Amount": "ID"},
		{"Id": "ID"},
	} {
		changed := regenerateRepairCandidate(w, m.cfg, table, trigger, mapping)
		require.Equal(t, "blocked", changed.Status)
		require.True(t, changed.DiagnosticOnly)
		require.Contains(t, changed.Error, "映射已改变")
		require.Equal(t, frozen, repair.Hash(changed.MySQLDamengTrigger))
		require.Equal(t, units, repair.Hash(changed.Units))
	}
}

type cancellingMySQLDMTriggerWriter struct {
	*mysqlDMTriggerWriter
	cancel context.CancelFunc
}

func (w *cancellingMySQLDMTriggerWriter) CreateMySQLDamengTrigger(ctx context.Context, plan mysqldmtrigger.Plan) error {
	err := w.mysqlDMTriggerWriter.CreateMySQLDamengTrigger(ctx, plan)
	if err == nil {
		w.cancel()
	}
	return err
}

func TestMySQLDamengCancelledTriggerWorkKeepsPendingInventory(t *testing.T) {
	m, r, w := mysqlDMTriggerFixture(t)
	m.cfg.CaptureRepair = true
	second, third := r.triggers[0], r.triggers[0]
	second.Name, second.ActionOrder = "Second", 2
	third.Name, third.ActionOrder = "Third", 3
	r.triggers = append(r.triggers, second, third)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.writer = &cancellingMySQLDMTriggerWriter{mysqlDMTriggerWriter: w, cancel: cancel}
	report := m.Run(ctx)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, 3, report.Triggers.Total)
	require.Equal(t, 1, report.Triggers.Success)
	require.Zero(t, report.Triggers.Failed)
	require.Len(t, w.plans, 1)
	pending := 0
	for _, object := range report.RepairManifest.Objects {
		if object.MySQLDamengTrigger == nil || object.Status == "succeeded" {
			continue
		}
		pending++
		require.Contains(t, []string{"pending", "blocked", "cancelled"}, object.Status)
		require.Len(t, object.Units, 1)
		require.Equal(t, "pending", object.Units[0].Status)
	}
	require.Equal(t, 2, pending)
}

func TestMySQLDamengRecoveryPreviewBlocksActiveTargetTriggers(t *testing.T) {
	m, r, w := mysqlDMTriggerFixture(t)
	m.cfg.TargetSchema = "DST"
	w.loadBlocked = "ORDERDATA"
	object := repair.Object{ID: "table", Kind: "table", SourceName: "OrderData", SourceTable: "OrderData", Table: "orderdata", TargetName: "orderdata", Schema: "DST", DataStatus: "failed", ColumnMapping: map[string]string{"Id": "ID", "Amount": "AMOUNT"}}
	ops := recoveryOps{
		schema: func(context.Context, target.Writer) (string, error) { return "DST", nil },
		inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
			return []source.ColumnInfo{{Name: "ID", DataType: "INT"}, {Name: "AMOUNT", DataType: "DECIMAL", IsNullable: true}}, nil
		},
	}
	_, err := previewRecovery(context.Background(), r, w, m.cfg, object, nil, nil, ops)
	require.ErrorContains(t, err, "启用触发器")
	require.Empty(t, w.copied)
	m.cfg.Content = "schema_only"
	preview, err := previewRecovery(context.Background(), r, w, m.cfg, object, nil, nil, ops)
	require.NoError(t, err)
	require.False(t, preview.DataRequired, "structure-only review must not be blocked by a data load guard")
}
