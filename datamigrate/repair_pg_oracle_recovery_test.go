package datamigrate

import (
	"context"
	"fmt"
	"testing"

	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type pgOracleRecoveryReaderFixture struct {
	*recoveryTestReader
	snapshot *source.PGSnapshot
	reads    int
}

func (*pgOracleRecoveryReaderFixture) DBType() string { return "postgres" }
func (r *pgOracleRecoveryReaderFixture) ReadPGOracleSnapshot(_ context.Context, tables []string) (*source.PGSnapshot, error) {
	if len(tables) != 1 || tables[0] != "SourceT" {
		return nil, fmt.Errorf("snapshot scope changed")
	}
	r.reads++
	return r.snapshot, nil
}
func (*pgOracleRecoveryReaderFixture) GetSequences(context.Context) ([]source.SequenceInfo, error) {
	return nil, fmt.Errorf("legacy GetSequences must not run")
}

type pgOracleRecoveryWriterFixture struct {
	*recoveryTestWriter
	configured    map[string][]source.ColumnInfo
	allowed       map[string]string
	validationErr error
	checked       []pgoracle.SequencePlan
}

func (*pgOracleRecoveryWriterFixture) DBType() string { return "oracle" }
func (w *pgOracleRecoveryWriterFixture) ConfigurePGOracle(cols map[string][]source.ColumnInfo) {
	w.configured = cols
}
func (w *pgOracleRecoveryWriterFixture) ValidatePGOracleDataTable(_ context.Context, _ string, _ []source.ColumnInfo, allowed map[string]string) error {
	w.allowed = allowed
	return w.validationErr
}
func (w *pgOracleRecoveryWriterFixture) InspectPGOracleSequence(_ context.Context, p pgoracle.SequencePlan) error {
	w.checked = append(w.checked, p)
	return nil
}
func (*pgOracleRecoveryWriterFixture) ValidatePGOracleSequenceBindings(context.Context, pgoracle.SequencePlan, bool) error {
	return nil
}
func (w *pgOracleRecoveryWriterFixture) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	if len(w.configured[table]) != len(cols) || w.configured[table][0].Name != cols[0] {
		return fmt.Errorf("PG column constraints not configured before copy")
	}
	return w.recoveryTestWriter.CopyData(ctx, table, cols, types, rows)
}

func pgOracleRecoveryFixture(t *testing.T) (*pgOracleRecoveryReaderFixture, *pgOracleRecoveryWriterFixture, Config, repair.Object, recoveryOps, *int) {
	r, w, cfg, obj, ops, truncated := recoveryFixture(t)
	r.ddl["SourceT"].Columns[0].DataType = "int4"
	obj.SourceColumns = r.ddl["SourceT"].Columns
	raw := &source.PGSnapshot{Profile: source.PGProfile{Schema: "Mixed"}, Tables: []source.PGTable{{Info: *r.ddl["SourceT"]}}}
	return &pgOracleRecoveryReaderFixture{recoveryTestReader: r, snapshot: raw}, &pgOracleRecoveryWriterFixture{recoveryTestWriter: w}, cfg, obj, ops, truncated
}

func TestPGOracleRecoveryUsesRawSnapshotAndConfiguresBeforeCopy(t *testing.T) {
	r, w, cfg, obj, ops, _ := pgOracleRecoveryFixture(t)
	r.snapshot.Tables[0].Unsupported = []source.PGObject{{Kind: "trigger", Name: "Business", Reason: "manual adaptation"}, {Kind: "routine", Name: "Func", Reason: "export only"}}
	p, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
	require.Equal(t, 1, r.reads)
	res, err := runRecovery(context.Background(), r, w, cfg, p, false, nil, ops)
	require.NoError(t, err)
	require.Equal(t, "succeeded", res.DataStatus)
	require.Equal(t, 2, r.reads)
	require.Equal(t, "int4", w.configured["TargetT"][0].DataType)
	require.Equal(t, "renamed", w.configured["TargetT"][0].Name)
	values, err := RecoverySequences(context.Background(), r, w, cfg, p)
	require.NoError(t, err)
	require.Empty(t, values)
	require.Equal(t, 3, r.reads)
}

func TestPGOracleRecoveryRejectsBusinessTriggerBeforeTruncate(t *testing.T) {
	r, w, cfg, obj, ops, truncated := pgOracleRecoveryFixture(t)
	w.validationErr = fmt.Errorf("business trigger is enabled")
	_, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "business trigger")
	require.Zero(t, *truncated)
	require.Zero(t, w.writes)
	require.Nil(t, w.configured)
}

func TestPGOracleRecoveryPreservesKnownSequenceIdentityAndChecksRawBindings(t *testing.T) {
	r, w, cfg, obj, ops, _ := pgOracleRecoveryFixture(t)
	seq := source.PGSequence{OID: 11, Name: "Seq", Schema: "Mixed", Increment: 1, Min: 1, Max: 1000, LastValue: 10, IsCalled: true, Cache: 1, Bindings: []source.PGSequenceBinding{{Schema: "Mixed", TableName: "SourceT", ColumnName: "ID", Owned: true}, {Schema: "Mixed", TableName: "Other", ColumnName: "ID"}}}
	plan := pgoracle.SequencePlan{Schema: "dst", Name: "SEQ", Source: seq, Bindings: []pgoracle.Binding{{SourceTable: "SourceT", SourceColumn: "ID", Table: "TargetT", Column: "renamed", Trigger: "TRG_ID"}, {SourceTable: "Other", SourceColumn: "ID", Table: "OTHER", Column: "ID", Trigger: "TRG_OTHER"}}}
	plan.Identity = &pgoracle.SequenceIdentity{ObjectID: 91, Fingerprint: plan.Fingerprint()}
	related := []repair.Object{{Kind: "sequence", Status: "succeeded", PGOracleSequence: &plan}}
	seq.LastValue = 20
	r.snapshot.Sequences = []source.PGSequence{seq}
	p, err := previewRecovery(context.Background(), r, w, cfg, obj, related, nil, ops)
	require.NoError(t, err)
	require.Len(t, p.PGOracleSequences, 1)
	require.Equal(t, int64(20), p.PGOracleSequences[0].Source.LastValue)
	require.Equal(t, int64(91), p.PGOracleSequences[0].Identity.ObjectID)
	require.Len(t, w.checked[0].Bindings, 2)
	require.Contains(t, w.allowed, "TRG_ID")
	require.Empty(t, p.Related)
	// A future source watermark is allowed only after a fresh preview. Any
	// changed dependency, including a different non-selected consumer, is not.
	r.snapshot.Sequences[0].Bindings = append(r.snapshot.Sequences[0].Bindings, source.PGSequenceBinding{Schema: "Outside", TableName: "NewConsumer", ColumnName: "ID"})
	_, err = previewRecovery(context.Background(), r, w, cfg, obj, related, nil, ops)
	require.ErrorContains(t, err, "完整绑定已变化")
}

func TestPGOracleRecoveryMissingSequenceLedgerBlocksBeforeMutation(t *testing.T) {
	r, w, cfg, obj, ops, truncated := pgOracleRecoveryFixture(t)
	r.snapshot.Sequences = []source.PGSequence{{Name: "Seq", Schema: "Mixed", Bindings: []source.PGSequenceBinding{{Schema: "Mixed", TableName: "SourceT", ColumnName: "ID"}}}}
	_, err := previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "缺少冻结工具方案")
	require.Zero(t, *truncated)
	require.Zero(t, w.writes)
}

func TestPGOracleRecoverySequenceInventoryFailureCannotHideKnownBinding(t *testing.T) {
	r, w, cfg, obj, ops, truncated := pgOracleRecoveryFixture(t)
	plan := pgoracle.SequencePlan{Source: source.PGSequence{Schema: "Mixed", Name: "Seq"}, Bindings: []pgoracle.Binding{{SourceTable: "SourceT", SourceColumn: "ID"}}}
	ctx := WithPGOracleRecoveryEvidence(context.Background(), []pgoracle.SequencePlan{plan})
	r.snapshot.Objects = []source.PGObject{{Kind: "sequence", Name: "序列元数据", Reason: "permission denied"}}
	_, err := previewRecovery(ctx, r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "补迁序列元数据不可确认")
	require.Zero(t, *truncated)
	require.Nil(t, w.configured)
	r.snapshot.Objects = nil
	_, err = previewRecovery(ctx, r, w, cfg, obj, nil, nil, ops)
	require.ErrorContains(t, err, "冻结绑定已不存在")
	// A regular table has no generator to calibrate and may still recover.
	r.snapshot.Objects = []source.PGObject{{Kind: "sequence", Name: "序列元数据", Reason: "permission denied"}}
	_, err = previewRecovery(context.Background(), r, w, cfg, obj, nil, nil, ops)
	require.NoError(t, err)
}

func TestPGOracleRecoveryDetectsSourceRecreation(t *testing.T) {
	saved := source.PGSequence{OID: 11, Name: "Seq", Schema: "Mixed", Increment: 1, Min: 1, Max: 100, LastValue: 10, Cache: 1}
	current := saved
	current.LastValue = 20
	require.NoError(t, comparePGOracleRecoverySequence(saved, current))
	current.OID = 12
	require.ErrorContains(t, comparePGOracleRecoverySequence(saved, current), "身份")
}

func TestPGOracleSequenceEvidenceIsDeepCopied(t *testing.T) {
	p := pgoracle.SequencePlan{Schema: "APP", Name: "SEQ", Identity: &pgoracle.SequenceIdentity{ObjectID: 91}, Bindings: []pgoracle.Binding{{Table: "T"}}, Source: source.PGSequence{Bindings: []source.PGSequenceBinding{{TableName: "Source"}}}}
	ctx := WithPGOracleRecoveryEvidence(context.Background(), []pgoracle.SequencePlan{p})
	p.Identity.ObjectID = 99
	p.Bindings[0].Table = "Changed"
	p.Source.Bindings[0].TableName = "Changed"
	first := PGOracleSequenceEvidence(ctx)
	require.Equal(t, int64(91), first[0].Identity.ObjectID)
	require.Equal(t, "T", first[0].Bindings[0].Table)
	require.Equal(t, "Source", first[0].Source.Bindings[0].TableName)
	first[0].Identity.ObjectID = 100
	require.Equal(t, int64(91), PGOracleSequenceEvidence(ctx)[0].Identity.ObjectID)
}
