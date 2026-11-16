package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type pgOracleSafetyWriter struct {
	*pgOracleFixtureWriter
	checkedEncoding bool
	activeScopes    []map[string]bool
}

func (w *pgOracleSafetyWriter) ValidatePGOracleStatements(_ context.Context, statements []dialect.Statement) error {
	for _, s := range statements {
		if strings.HasPrefix(s.SQL, `CREATE TABLE "APP"."AENCODING"`) {
			w.checkedEncoding = true
			return fmt.Errorf("SQL 文本含 Oracle 目标字符集无法表示的字符")
		}
	}
	return nil
}

func (w *pgOracleSafetyWriter) PreflightOracle(_ context.Context, tables map[string]*source.TableDDLInfo, _ []target.OracleObject, _ []source.SequenceInfo, rebuild, _ bool) error {
	if len(tables) == 0 {
		return nil
	}
	scope := map[string]bool{}
	for name := range tables {
		scope[name] = true
	}
	w.activeScopes = append(w.activeScopes, scope)
	// Model Oracle's old A -> B foreign key: B can only be rebuilt while A is
	// itself in the rebuild batch, otherwise DROP CASCADE would change retained A.
	if rebuild && scope["BPARENT"] && !scope["AENCODING"] {
		return &target.OracleTablePreflightError{Table: "BPARENT", Err: fmt.Errorf("保留表 AENCODING 的外键引用 BPARENT")}
	}
	return nil
}

func TestPGOracleSQLPreflightIsolationPrecedesRetainedDependencyClosure(t *testing.T) {
	r, base, m := pgOracleFixture("AEncoding", "BParent", "ZIndependent")
	w := &pgOracleSafetyWriter{pgOracleFixtureWriter: base}
	m.writer = w
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.True(t, w.checkedEncoding)
	require.GreaterOrEqual(t, len(w.activeScopes), 2, "removing B must recheck the remaining batch")
	for _, scope := range w.activeScopes {
		require.False(t, scope["AENCODING"], "all final SQL checks must precede retained-target closure")
	}
	require.Equal(t, 2, report.Tables.Failed)
	require.Equal(t, 1, report.Tables.Success)
	require.EqualValues(t, 2, w.counts["ZINDEPENDENT"])
	for _, name := range []string{"AEncoding", "BParent"} {
		require.NotContains(t, r.reads, name)
		require.Zero(t, w.copyAttempts[strings.ToUpper(name)])
		for _, sql := range w.statements {
			require.NotContains(t, sql, `"APP"."`+strings.ToUpper(name)+`"`)
		}
	}
}

func TestPGOracleObjectSinkWithoutManifestPreventsEveryWrite(t *testing.T) {
	r, w, m := pgOracleFixture("Orders")
	m.cfg.CaptureRepair = true
	objectWrites := 0
	m.cfg.RepairObjectSink = func(repair.Object) error { objectWrites++; return nil }
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Zero(t, objectWrites, "no partial ledger can authorize execution")
	require.Empty(t, w.statements)
	require.Empty(t, w.copied)
	require.Empty(t, w.created)
	require.Empty(t, r.reads)
}

func TestPGOracleMissingGeneratorRemainsObjectFailure(t *testing.T) {
	r, w, m := pgOracleFixture("Orders", "Independent")
	r.snapshot.Tables[0].Info.Columns[0].Identity = "a"
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, report.Sequences.Failed)
	require.Contains(t, report.Sequences.Items[0].Error, "缺少可核验")
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 2, report.Data.Success)
	require.EqualValues(t, 2, w.counts["ORDERS"])
	require.EqualValues(t, 2, w.counts["INDEPENDENT"])
}

func TestPGOracleUnmappedSequenceSchemaDoesNotBlockHealthyData(t *testing.T) {
	r, w, m := pgOracleFixture("Orders", "Independent")
	def := `nextval('external.serial'::regclass)`
	r.snapshot.Tables[0].Info.Columns[0].Default = &def
	r.snapshot.Sequences = []source.PGSequence{{Name: "serial", Schema: "external", Increment: 1, Min: 1, Max: 10000, LastValue: 1, Cache: 1, Bindings: []source.PGSequenceBinding{{Schema: "MixedSchema", TableName: "Orders", ColumnName: "Id", Default: def}}}}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, report.Sequences.Failed)
	require.Equal(t, 1, report.Triggers.Failed)
	require.Equal(t, 2, report.Data.Success)
	require.Contains(t, report.Sequences.Items[0].Error, "未映射的源 Schema")
	require.EqualValues(t, 2, w.counts["ORDERS"])
	for _, sql := range w.statements {
		require.NotContains(t, sql, "CREATE SEQUENCE")
	}
}
