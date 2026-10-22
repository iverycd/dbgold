package datamigrate

import (
	"context"
	"errors"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type nativeVariableTestWriter struct {
	*triggerTestWriter
	supported bool
	probeErr  error
	probes    int
}

func (*nativeVariableTestWriter) DBType() string { return "kingbase" }
func (*nativeVariableTestWriter) Dialect() dialect.Dialect {
	return dialect.NewPostgres("kingbase")
}
func (w *nativeVariableTestWriter) DetectMySQLPGNativeUserVariables(context.Context) (bool, error) {
	w.probes++
	return w.supported, w.probeErr
}

func nativeVariableMigrationFixture(t *testing.T) (*Migrator, *triggerCharacterTestReader, *nativeVariableTestWriter) {
	t.Helper()
	m, r, w := triggerFixture(t)
	r.triggers[0].Timing, r.triggers[0].Event = "BEFORE", "UPDATE"
	r.triggers[0].Body = "SET @CurrentValue = OLD.Id + 1"
	r.triggers[0].SourceDDL = "CREATE TRIGGER FillAmount BEFORE UPDATE ON OrderData FOR EACH ROW SET @CurrentValue = OLD.Id + 1"
	reader := &triggerCharacterTestReader{triggerTestReader: r, characters: &source.TriggerCharacterMetadata{
		ServerVersion:  "8.0.42 Source distribution",
		NumericColumns: []source.TriggerNumericColumn{{Schema: "App", Table: "OrderData", Name: "Id", ColumnType: "int"}},
	}}
	writer := &nativeVariableTestWriter{triggerTestWriter: w, supported: true}
	m.reader, m.writer = reader, writer
	return m, reader, writer
}

func TestNativeVariablePreparationFreezesCapabilityAndUsedMetadata(t *testing.T) {
	m, _, w := nativeVariableMigrationFixture(t)
	m.prepareMySQLPGTriggers(context.Background())
	require.NoError(t, m.mysqlPGTriggerError)
	require.Len(t, m.mysqlPGTriggers, 1)
	p := m.mysqlPGTriggers[0]
	require.NoError(t, p.err)
	require.True(t, p.plan.NativeUserVariables)
	require.Equal(t, mysqltrigger.CurrentVersion, p.plan.Version)
	require.Len(t, p.plan.CharacterMetadata.NumericColumns, 1)
	require.NotEmpty(t, p.plan.Tables)
	require.NoError(t, mysqltrigger.ValidatePlan(p.plan))
	require.Equal(t, 1, w.probes)
	require.Empty(t, w.plans, "preparation cannot execute target DDL")
}

func TestNativeVariableCapabilityFailureOnlyBlocksDependentTriggers(t *testing.T) {
	for _, probeErr := range []error{nil, errors.New("target permission denied")} {
		t.Run(map[bool]string{true: "unavailable", false: "probe error"}[probeErr == nil], func(t *testing.T) {
			m, r, w := nativeVariableMigrationFixture(t)
			ordinary := r.triggers[0]
			ordinary.Name, ordinary.Timing, ordinary.Event = "Ordinary", "BEFORE", "INSERT"
			ordinary.Body = "SET NEW.Id = 1"
			r.triggers = append(r.triggers, ordinary)
			w.supported, w.probeErr = false, probeErr
			m.prepareMySQLPGTriggers(context.Background())
			require.NoError(t, m.mysqlPGTriggerError)
			require.Len(t, m.mysqlPGTriggers, 2)
			require.Error(t, m.mysqlPGTriggers[0].err)
			if probeErr != nil {
				require.ErrorContains(t, m.mysqlPGTriggers[0].err, probeErr.Error())
			}
			require.NoError(t, m.mysqlPGTriggers[1].err)
			require.False(t, m.mysqlPGTriggers[1].plan.NativeUserVariables)
		})
	}
}

func TestNativeVariableRepairPlanPreservesOriginalFailure(t *testing.T) {
	m, r, w := nativeVariableMigrationFixture(t)
	info := r.triggers[0]
	original := repair.Object{ID: "native-trigger", Kind: "trigger", Schema: m.cfg.TargetSchema,
		Table: m.objName(info.TableName), TargetName: m.objName(info.Name), SourceTable: info.TableName,
		SourceName: info.Name, SourceDDL: info.SourceDDL, Status: "failed", OriginalStatus: "failed",
		Error: "仅支持 SET NEW.列 = 表达式", OriginalError: "仅支持 SET NEW.列 = 表达式", DiagnosticOnly: true}
	candidate, err := BuildMySQLPGTriggerRepairPlan(context.Background(), r, w, m.cfg, original, nil)
	require.NoError(t, err)
	require.Equal(t, "pending", candidate.Status)
	require.Equal(t, original.OriginalError, candidate.OriginalError)
	require.Equal(t, original.SourceDDL, candidate.SourceDDL)
	require.True(t, candidate.MySQLPGTrigger.NativeUserVariables)
	require.Len(t, candidate.Units, 2)
	require.Contains(t, candidate.MySQLPGTrigger.FunctionBody, "@CurrentValue")
	require.Empty(t, w.plans)
	// Capabilities are checked anew when a user regenerates an old report.
	w.supported = false
	_, err = BuildMySQLPGTriggerRepairPlan(context.Background(), r, w, m.cfg, original, nil)
	require.Error(t, err)
	w.supported = true
	r.triggers[0].SourceDDL += " -- changed"
	_, err = BuildMySQLPGTriggerRepairPlan(context.Background(), r, w, m.cfg, original, nil)
	require.ErrorContains(t, err, "DDL 已改变")
}
