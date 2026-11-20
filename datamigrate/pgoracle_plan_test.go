package datamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/pgoracle"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type pgOracleFixtureReader struct {
	*mockReader
	snapshot    *source.PGSnapshot
	snapshotErr error
	reads       []string
	requested   [][]string
}

func (*pgOracleFixtureReader) DBType() string                { return "postgres" }
func (r *pgOracleFixtureReader) PGProfile() source.PGProfile { return r.snapshot.Profile }
func (r *pgOracleFixtureReader) ReadPGOracleSnapshot(_ context.Context, tables []string) (*source.PGSnapshot, error) {
	r.requested = append(r.requested, slices.Clone(tables))
	return r.snapshot, r.snapshotErr
}
func (r *pgOracleFixtureReader) ReadPage(_ context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	r.reads = append(r.reads, table)
	info := r.ddl[table]
	if info == nil {
		return nil, nil, nil, fmt.Errorf("unknown source table %q", table)
	}
	var columns, types []string
	for _, c := range info.Columns {
		columns = append(columns, c.Name)
		types = append(types, c.DataType)
	}
	rows := r.rows[table]
	if offset >= int64(len(rows)) {
		return columns, types, nil, nil
	}
	end := min(int64(len(rows)), offset+limit)
	return columns, types, rows[offset:end], nil
}
func (r *pgOracleFixtureReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}

type pgOracleFixtureWriter struct {
	mockWriter
	dia                 *dialect.OracleDialect
	counts              map[string]int64
	createFailure       string
	createFailureStage  string
	createError         error
	copyFailure         string
	copyFailurePage     int
	copyAttempts        map[string]int
	copyColumns         map[string][]string
	statements          []string
	objects             []string
	configured          map[string][]source.ColumnInfo
	validated           []string
	missing             map[string]bool
	profileError        error
	preflightError      error
	sequenceError       error
	sequenceInspections int
	objectFailure       string
	dependencies        []target.MigrationDependency
	allowedTriggers     map[string]map[string]string
}

func (*pgOracleFixtureWriter) DBType() string             { return "oracle" }
func (w *pgOracleFixtureWriter) Dialect() dialect.Dialect { return w.dia }
func (*pgOracleFixtureWriter) EffectiveSchema() string    { return "APP" }
func (w *pgOracleFixtureWriter) InspectMigrationDependencies(context.Context) (target.MigrationDependencies, error) {
	return target.MigrationDependencies{Schema: "APP", Relations: w.dependencies}, nil
}
func (w *pgOracleFixtureWriter) InspectPGOracle(context.Context) (target.PGOracleProfile, error) {
	return target.PGOracleProfile{Product: "oracle", Version: "11.2.0.4", Schema: "APP", Charset: "AL32UTF8", NCharset: "AL16UTF16", Verification: "fixture"}, w.profileError
}
func (w *pgOracleFixtureWriter) PreflightOracle(context.Context, map[string]*source.TableDDLInfo, []target.OracleObject, []source.SequenceInfo, bool, bool) error {
	return w.preflightError
}
func (w *pgOracleFixtureWriter) ValidatePGOracleDataTable(_ context.Context, name string, _ []source.ColumnInfo, allowed map[string]string) error {
	w.validated = append(w.validated, name)
	if w.allowedTriggers == nil {
		w.allowedTriggers = map[string]map[string]string{}
	}
	w.allowedTriggers[name] = allowed
	if w.missing[name] {
		return errors.New("target table is missing")
	}
	return nil
}
func (*pgOracleFixtureWriter) ValidatePGOracleSequenceBindings(context.Context, pgoracle.SequencePlan, bool) error {
	return nil
}
func (w *pgOracleFixtureWriter) InspectPGOracleSequence(context.Context, pgoracle.SequencePlan) error {
	w.sequenceInspections++
	return w.sequenceError
}
func (w *pgOracleFixtureWriter) PGOracleRelationExists(_ context.Context, name string) (bool, error) {
	return !w.missing[name], nil
}
func (w *pgOracleFixtureWriter) PGOracleSequenceIdentity(_ context.Context, p pgoracle.SequencePlan) (*pgoracle.SequenceIdentity, error) {
	return &pgoracle.SequenceIdentity{ObjectID: 42, Fingerprint: p.Fingerprint()}, nil
}
func (w *pgOracleFixtureWriter) ConfigurePGOracle(columns map[string][]source.ColumnInfo) {
	w.configured = columns
}
func (w *pgOracleFixtureWriter) CountRows(_ context.Context, name string) (int64, error) {
	return w.counts[name], nil
}
func (w *pgOracleFixtureWriter) execute(ctx context.Context, statements []dialect.Statement) error {
	for _, statement := range statements {
		if err := repair.Observe(ctx, statement.SQL, "running", nil); err != nil {
			return err
		}
		w.statements = append(w.statements, statement.SQL)
		var err error
		if w.createFailure != "" && strings.Contains(statement.SQL, `"APP"."`+w.createFailure+`"`) && strings.HasPrefix(statement.SQL, w.createFailureStage+" TABLE ") {
			err = w.createError
			if err == nil {
				err = errors.New("injected DDL failure")
			}
		}
		status := "succeeded"
		if err != nil {
			status = "failed"
		}
		if observed := repair.Observe(ctx, statement.SQL, status, err); observed != nil {
			return observed
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (w *pgOracleFixtureWriter) CreateTable(ctx context.Context, statements []dialect.Statement) error {
	if err := w.execute(ctx, statements); err != nil {
		return err
	}
	w.created = append(w.created, dialect.JoinSQL(statements))
	return nil
}
func (w *pgOracleFixtureWriter) ExecutePGOracleStatements(ctx context.Context, kind, name string, statements []dialect.Statement) error {
	w.objects = append(w.objects, kind+":"+name)
	if w.objectFailure == kind+":"+name {
		return errors.New("injected object failure")
	}
	return w.execute(ctx, statements)
}
func (w *pgOracleFixtureWriter) CopyData(_ context.Context, table string, cols, _ []string, rows [][]any) error {
	w.copyAttempts[table]++
	w.copyColumns[table] = slices.Clone(cols)
	if table == w.copyFailure && w.copyAttempts[table] == w.copyFailurePage {
		return errors.New("invalid row value")
	}
	w.copied = append(w.copied, table)
	w.counts[table] += int64(len(rows))
	return nil
}

func pgOracleFixture(names ...string) (*pgOracleFixtureReader, *pgOracleFixtureWriter, *Migrator) {
	r := &pgOracleFixtureReader{mockReader: &mockReader{tables: slices.Clone(names), ddl: map[string]*source.TableDDLInfo{}, pk: map[string][]string{}, rows: map[string][][]any{}}, snapshot: &source.PGSnapshot{Profile: source.PGProfile{Product: "postgres", Version: "16.1", Schema: "MixedSchema", Verification: "fixture"}}}
	for _, name := range names {
		info := source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "Id", DataType: "int4"}}}
		r.ddl[name] = &info
		r.pk[name] = []string{"Id"}
		r.rows[name] = [][]any{{int64(1)}, {int64(2)}}
		r.snapshot.Tables = append(r.snapshot.Tables, source.PGTable{Kind: "r", Info: info, Indexes: []source.IndexInfo{{TableName: name, IndexName: "PK_" + name, Columns: []string{"Id"}, IsPrimary: true, IsUnique: true, Method: "btree", PG: &source.PGIndexMetadata{KeyCount: 1, Valid: true, Immediate: true, ConstraintName: "PK_" + name, ConstraintType: "p", Validated: true}}}})
	}
	w := &pgOracleFixtureWriter{dia: dialect.NewOracle(), counts: map[string]int64{}, copyAttempts: map[string]int{}, copyColumns: map[string][]string{}, missing: map[string]bool{}, createFailureStage: "CREATE"}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, Config{TargetDBType: "oracle", TargetSchema: "APP", Mode: "all", Content: "both", PageSize: 1, MaxParallel: 1, LowerCaseNames: true})
	return r, w, m
}

func TestPGOraclePreflightReadOnlyAndOriginalSourceNames(t *testing.T) {
	r, w, m := pgOracleFixture("MixedTable")
	before, err := json.Marshal(r.snapshot)
	require.NoError(t, err)
	p := m.PreflightPGOracle(context.Background())
	require.True(t, p.Ready, p.Issues)
	require.True(t, p.CanStart)
	require.Equal(t, "pg_oracle", p.Kind)
	require.Equal(t, "structure", p.CheckScope)
	require.Equal(t, "postgres", p.Source.Product)
	require.Equal(t, "APP", p.Target.Schema)
	require.Equal(t, [][]string{{"MixedTable"}}, r.requested)
	require.Empty(t, r.reads)
	require.Empty(t, w.statements)
	require.Empty(t, w.copied)
	require.Equal(t, "MixedTable", p.Objects[0].Object.Name)
	require.Equal(t, "MIXEDTABLE", p.Objects[0].Target)
	require.Contains(t, dialect.JoinSQL(p.Objects[0].Statements), `"APP"."MIXEDTABLE"`)
	after, err := json.Marshal(r.snapshot)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
}

func TestPGOracleOnlyCatalogViewWithUnreadableDDLIsEditable(t *testing.T) {
	r, w, m := pgOracleFixture()
	r.snapshot.Views = []source.ViewInfo{{ViewName: "HiddenView", Unsupported: "视图定义不可读取"}, {ViewName: "迁移准备", Unsupported: "视图定义不可读取"}}
	r.snapshot.Objects = []source.PGObject{{Kind: "view", Name: "视图元数据", Reason: "permission denied"}}
	m.cfg.CaptureRepair = true
	m.cfg.ViewNames = []string{"HiddenView", "迁移准备"}
	report := m.Run(t.Context())
	require.Equal(t, 3, report.Views.Failed)
	require.Empty(t, w.statements)
	require.Len(t, report.RepairManifest.Objects, 3)
	for _, item := range report.RepairManifest.Objects {
		require.Empty(t, item.Units)
		require.Empty(t, item.SourceDDL)
		switch item.SourceName {
		case "HiddenView", "迁移准备":
			require.True(t, item.CanEditDDL())
			require.Equal(t, "failed", item.Status)
			require.Equal(t, "视图定义不可读取", item.OriginalError)
		case "视图元数据":
			require.True(t, item.DiagnosticOnly)
			require.False(t, item.CanEditDDL())
		default:
			t.Fatalf("unexpected repair object %q", item.SourceName)
		}
	}
}

func TestPGOraclePreflightCanStartHealthySubset(t *testing.T) {
	r, _, m := pgOracleFixture("Bad", "Good")
	r.snapshot.Tables[0].Info.Columns[0].DataType = "_int4"
	p := m.PreflightPGOracle(context.Background())
	require.False(t, p.Ready)
	require.True(t, p.CanStart)
	require.Error(t, p.dependencyError(tableRef("Bad"), map[ObjectRef]bool{}))
	require.NoError(t, p.dependencyError(tableRef("Good"), map[ObjectRef]bool{}))
	r2, w2, m2 := pgOracleFixture("OnlyBad")
	r2.snapshot.Tables[0].Info.Columns[0].DataType = "_int4"
	p2 := m2.PreflightPGOracle(context.Background())
	require.False(t, p2.Ready)
	require.False(t, p2.CanStart)
	require.Empty(t, w2.statements)
	_, w3, m3 := pgOracleFixture("Healthy")
	w3.profileError = errors.New("cannot inspect target")
	p3 := m3.PreflightPGOracle(context.Background())
	require.False(t, p3.Ready)
	require.False(t, p3.CanStart)
	require.True(t, p3.globalError)
}

func TestPGOraclePreflightNameCollisionsBlockOnlyAffectedTables(t *testing.T) {
	_, _, m := pgOracleFixture("Orders", "ORDERS", "Healthy")
	p := m.PreflightPGOracle(context.Background())
	require.False(t, p.Ready)
	require.True(t, p.CanStart)
	for _, name := range []string{"Orders", "ORDERS"} {
		require.ErrorContains(t, p.blocked[tableRef(name)], "名称冲突")
	}
	require.NoError(t, p.blocked[tableRef("Healthy")])
	r, _, m := pgOracleFixture("Collision", "Healthy")
	r.snapshot.Tables[0].Info.Columns = append(r.snapshot.Tables[0].Info.Columns, source.ColumnInfo{Name: "ID", DataType: "int4"})
	p = m.PreflightPGOracle(context.Background())
	require.ErrorContains(t, p.blocked[tableRef("Collision")], "列名")
	require.True(t, p.CanStart)
}

func TestPGOraclePreflightSpecialIndexesAndForeignKeyDependencies(t *testing.T) {
	r, _, m := pgOracleFixture("Parent", "Child")
	parent := &r.snapshot.Tables[0]
	parent.Info.Columns = append(parent.Info.Columns, source.ColumnInfo{Name: "Optional", DataType: "int4", IsNullable: true})
	parent.Indexes = append(parent.Indexes,
		source.IndexInfo{TableName: "Parent", IndexName: "IxGin", Columns: []string{"Id"}, Method: "gin", Definition: `CREATE INDEX "IxGin" ON "Parent" USING gin ("Id")`},
		source.IndexInfo{TableName: "Parent", IndexName: "UqPair", Columns: []string{"Id", "Optional"}, IsUnique: true, Method: "btree", PG: &source.PGIndexMetadata{KeyCount: 2, Valid: true, ConstraintName: "UqPair", ConstraintType: "u", Validated: true}},
	)
	r.snapshot.Tables[1].Info.Columns = append(r.snapshot.Tables[1].Info.Columns, source.ColumnInfo{Name: "Optional", DataType: "int4", IsNullable: true})
	r.snapshot.Tables[1].ForeignKeys = []source.FKInfo{{TableName: "Child", ConstraintName: "FkPair", Columns: []string{"Id", "Optional"}, RefSchema: "MixedSchema", RefTable: "Parent", RefColumns: []string{"Id", "Optional"}, OnDelete: "NO ACTION", OnUpdate: "NO ACTION", PG: &source.PGConstraintMetadata{Match: "s", Validated: true}}}
	p := m.PreflightPGOracle(context.Background())
	require.False(t, p.Ready)
	require.True(t, p.CanStart)
	for _, name := range []string{"Parent", "Child"} {
		require.NoError(t, p.dependencyError(tableRef(name), map[ObjectRef]bool{}))
	}
	require.ErrorContains(t, p.blocked[ObjectRef{Kind: "index", Table: "Parent", Name: "IxGin"}], "BTREE")
	require.ErrorContains(t, p.blocked[ObjectRef{Kind: "index", Table: "Parent", Name: "UqPair"}], "NULL")
	require.ErrorContains(t, p.dependencyError(ObjectRef{Kind: "foreign_key", Table: "Child", Name: "FkPair"}, map[ObjectRef]bool{}), "引用键")
}

func TestPGOracleObjectCheckAndViewPreflightScopes(t *testing.T) {
	for _, mode := range []string{"check", "view"} {
		t.Run(mode, func(t *testing.T) {
			r, w, m := pgOracleFixture("Orders")
			r.snapshot.Tables[0].Checks = []source.PGObject{{Kind: "check", Table: "Orders", Name: "Positive", Definition: `CHECK ("Id" > 0)`, PG: &source.PGConstraintMetadata{Type: "c", Validated: true}}}
			r.snapshot.Views = []source.ViewInfo{{ViewName: "OrderView", Definition: `SELECT "Id" FROM "MixedSchema"."Orders"`, Columns: []source.ColumnInfo{{Name: "Id", DataType: "int4"}}}}
			if mode == "check" {
				m.cfg.Objects = []string{"check_constraints"}
				m.cfg.TableNames = []string{"Orders"}
			} else {
				m.cfg.ViewNames = []string{"OrderView"}
			}
			p := m.PreflightPGOracle(context.Background())
			require.True(t, p.Ready, p.Issues)
			require.True(t, p.CanStart)
			require.Len(t, p.Objects, 1)
			require.Equal(t, mode, p.Objects[0].Object.Kind)
			require.Empty(t, r.reads)
			require.Empty(t, w.statements)
		})
	}
}
