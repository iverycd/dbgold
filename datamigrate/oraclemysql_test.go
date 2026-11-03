package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type oracleMySQLTestReader struct {
	*mockReader
	keys    []source.IndexInfo
	indexes []source.IndexInfo
	fks     []source.FKInfo
	reads   map[string]int
	readMu  sync.Mutex
}

func (r *oracleMySQLTestReader) DBType() string               { return "oracle" }
func (r *oracleMySQLTestReader) MigrationSchema() string      { return "APP" }
func (r *oracleMySQLTestReader) MetadataReadConcurrency() int { return 4 }
func (r *oracleMySQLTestReader) GetTableDDLInfo(_ context.Context, table string) (*source.TableDDLInfo, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.reads == nil {
		r.reads = map[string]int{}
	}
	r.reads[table]++
	return r.ddl[table], nil
}
func (r *oracleMySQLTestReader) GetPrimaryKeys(context.Context) ([]source.IndexInfo, error) {
	return r.keys, nil
}
func (r *oracleMySQLTestReader) GetIndexes(context.Context) ([]source.IndexInfo, error) {
	return r.indexes, nil
}
func (r *oracleMySQLTestReader) GetForeignKeys(context.Context) ([]source.FKInfo, error) {
	return r.fks, nil
}
func (r *oracleMySQLTestReader) ReadPage(_ context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	var cols, types []string
	for _, col := range r.ddl[table].Columns {
		cols, types = append(cols, col.Name), append(types, col.DataType)
	}
	data := r.rows[table]
	if offset >= int64(len(data)) {
		return cols, types, nil, nil
	}
	return cols, types, data[offset:min(offset+limit, int64(len(data)))], nil
}
func (r *oracleMySQLTestReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.rows[table])), nil
}

type oracleMySQLTestWriter struct {
	*mockWriter
	mu             sync.Mutex
	fks            []target.MySQLForeignKey
	version        dialect.MySQLVersion
	versionCalls   int
	versionError   error
	caseMode       int
	inspectCalls   int
	inspectError   error
	drift          bool
	dependencyFail string
	createFail     string
	indexFail      string
	copyFail       string
	cancelOnIndex  func()
	events         []string
	foreignKeys    []source.FKInfo
	counts         map[string]int64
}

func (w *oracleMySQLTestWriter) DBType() string           { return "mysql" }
func (w *oracleMySQLTestWriter) Dialect() dialect.Dialect { return dialect.NewMySQL() }
func (w *oracleMySQLTestWriter) OracleMySQLSchema(context.Context) (string, error) {
	return "dst", nil
}
func (w *oracleMySQLTestWriter) OracleMySQLLowerCaseTableNames(context.Context) (int, error) {
	return w.caseMode, nil
}
func (w *oracleMySQLTestWriter) OracleMySQLVersion(context.Context) (dialect.MySQLVersion, error) {
	w.versionCalls++
	return w.version, w.versionError
}
func (w *oracleMySQLTestWriter) OracleMySQLForeignKeys(context.Context) ([]target.MySQLForeignKey, error) {
	w.inspectCalls++
	if w.inspectError != nil {
		return nil, w.inspectError
	}
	result := slices.Clone(w.fks)
	if w.drift && w.inspectCalls > 1 {
		result = append(result, testOracleMySQLTargetFK("outside", "retained", "dst", "P", "drift"))
	}
	return result, nil
}
func (w *oracleMySQLTestWriter) ExecuteOracleMySQLDependency(ctx context.Context, sql string) error {
	w.events = append(w.events, sql)
	if err := repair.Observe(ctx, sql, "running", nil); err != nil {
		return err
	}
	if sql == w.dependencyFail {
		err := errors.New("foreign key cleanup denied")
		return errors.Join(err, repair.Observe(ctx, sql, "failed", err))
	}
	return repair.Observe(ctx, sql, "succeeded", nil)
}
func (w *oracleMySQLTestWriter) CreateTable(ctx context.Context, stmts []dialect.Statement) error {
	for _, stmt := range stmts {
		w.events = append(w.events, stmt.SQL)
		if err := repair.Observe(ctx, stmt.SQL, "running", nil); err != nil {
			return err
		}
		if strings.HasPrefix(stmt.SQL, "CREATE TABLE") && w.createFail != "" && strings.Contains(stmt.SQL, "`"+w.createFail+"`") {
			err := errors.New("create denied")
			return errors.Join(err, repair.Observe(ctx, stmt.SQL, "failed", err))
		}
		if err := repair.Observe(ctx, stmt.SQL, "succeeded", nil); err != nil {
			return err
		}
	}
	w.created = append(w.created, dialect.JoinSQL(stmts))
	return nil
}
func (w *oracleMySQLTestWriter) CopyData(_ context.Context, table string, _ []string, _ []string, rows [][]any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if table == w.copyFail {
		return errors.New("copy denied")
	}
	w.counts[table] += int64(len(rows))
	return nil
}
func (w *oracleMySQLTestWriter) CountRows(_ context.Context, table string) (int64, error) {
	return w.counts[table], nil
}
func (w *oracleMySQLTestWriter) CreateIndex(_ context.Context, index source.IndexInfo) error {
	w.events = append(w.events, "index "+index.TableName)
	if w.cancelOnIndex != nil {
		w.cancelOnIndex()
	}
	if index.TableName == w.indexFail {
		return errors.New("key denied")
	}
	return nil
}
func (w *oracleMySQLTestWriter) CreateForeignKey(_ context.Context, fk source.FKInfo) error {
	w.events = append(w.events, "foreign_key "+fk.TableName)
	w.foreignKeys = append(w.foreignKeys, fk)
	return nil
}

func testOracleMySQLTargetFK(schema, table, refSchema, parent, name string) target.MySQLForeignKey {
	d := dialect.NewMySQL()
	fk := target.MySQLForeignKey{Schema: schema, Table: table, ConstraintName: name, RefSchema: refSchema, RefTable: parent, Columns: []string{"ID"}, RefColumns: []string{"ID"}, OnDelete: "RESTRICT", OnUpdate: "RESTRICT"}
	fk.DropSQL = "ALTER TABLE " + d.QualifyTable(schema, table) + " DROP FOREIGN KEY " + d.QuoteIdent(name)
	fk.RestoreSQL = "ALTER TABLE " + d.QualifyTable(schema, table) + " ADD CONSTRAINT " + d.QuoteIdent(name) + " FOREIGN KEY (`ID`) REFERENCES " + d.QualifyTable(refSchema, parent) + " (`ID`)"
	return fk
}

func oracleMySQLFixture(t *testing.T, names ...string) (*Migrator, *oracleMySQLTestReader, *oracleMySQLTestWriter) {
	t.Helper()
	r := &oracleMySQLTestReader{mockReader: &mockReader{tables: names, ddl: map[string]*source.TableDDLInfo{}, pk: map[string][]string{}, rows: map[string][][]any{}}}
	for _, name := range names {
		r.ddl[name] = &source.TableDDLInfo{TableName: name, Columns: []source.ColumnInfo{{Name: "ID", DataType: "NUMBER", Precision: 9}}}
		r.keys = append(r.keys, source.IndexInfo{TableName: name, IndexName: "PRIMARY", Columns: []string{"ID"}, IsPrimary: true, IsUnique: true})
		r.pk[name] = []string{"ID"}
	}
	w := &oracleMySQLTestWriter{mockWriter: &mockWriter{}, version: dialect.MySQLVersion{Major: 8, Minor: 0, Patch: 30}, counts: map[string]int64{}}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, Config{TargetDBType: "mysql", TargetSchema: "dst", Mode: "include", Filter: strings.Join(names, ","), Content: "schema_only", PageSize: 2, MaxParallel: 2})
	return m, r, w
}

func TestOracleMySQLDefaultsUseSamePlanInDDLAndRepair(t *testing.T) {
	m, r, w := oracleMySQLFixture(t, "frame_app_class")
	bareNull, stringNull := "(NULL)", "'NULL'"
	r.ddl["frame_app_class"].Columns = []source.ColumnInfo{
		{Name: "appclassguid", DataType: "VARCHAR2", Length: 50},
		{Name: "appclassname", DataType: "VARCHAR2", Length: 50, IsNullable: true, Default: &stringNull},
		{Name: "ordernumber", DataType: "NUMBER", Default: &bareNull},
		{Name: "tenantguid", DataType: "VARCHAR2", Length: 50, IsNullable: true},
	}
	r.keys = nil
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Len(t, w.created, 1)
	require.Contains(t, w.created[0], "`ordernumber` double NOT NULL")
	require.NotContains(t, w.created[0], "NOT NULL DEFAULT NULL")
	require.Contains(t, w.created[0], "`appclassname` varchar(50) DEFAULT 'NULL'")
	require.Zero(t, w.versionCalls, "ordinary tables must not acquire a version probe")
	require.NotNil(t, report.RepairManifest)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "table" {
			require.Equal(t, w.created[0], object.DDL)
			require.Equal(t, "NUMBER", object.SourceColumns[2].DataType)
			require.Equal(t, "(NULL)", *object.SourceColumns[2].Default)
			require.Equal(t, "double", object.Columns[2].DataType)
			require.Nil(t, object.Columns[2].Default)
		}
	}
	require.Equal(t, "NUMBER", r.ddl["frame_app_class"].Columns[2].DataType)
}

func TestOracleMySQLWidePlanPreservesSourceAndManifest(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%v", capture), func(t *testing.T) {
			m, r, w := oracleMySQLIndexFixture(t, "Wide")
			for i := 0; i < 10; i++ {
				r.ddl["Wide"].Columns = append(r.ddl["Wide"].Columns, source.ColumnInfo{Name: fmt.Sprintf("C%d", i), DataType: "VARCHAR2", Length: 2000, IsNullable: true})
			}
			r.indexes = []source.IndexInfo{{TableName: "Wide", IndexName: "Protected", Columns: []string{"C0"}}}
			m.cfg.LowerCaseNames, m.cfg.CaptureRepair = true, capture
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.Equal(t, 1, w.versionCalls)
			require.Len(t, report.AutoRepairs, 2)
			require.Contains(t, w.created[0], "`c0` varchar(2000)")
			require.Contains(t, w.created[0], "`c1` longtext")
			require.Contains(t, w.created[0], "CHECK (CHAR_LENGTH(`c1`) <= 2000)")
			require.Equal(t, "VARCHAR2", r.ddl["Wide"].Columns[2].DataType)
			if capture {
				for _, object := range report.RepairManifest.Objects {
					if object.Kind == "table" {
						require.Equal(t, w.created[0], object.DDL)
						require.Equal(t, "longtext", object.Columns[2].DataType)
						require.Equal(t, "VARCHAR2", object.SourceColumns[2].DataType)
					}
				}
			}
		})
	}
}

func TestOracleMySQLForeignKeysCleanupCyclesAndInclude(t *testing.T) {
	m, r, w := oracleMySQLFixture(t, "P", "C", "S")
	w.fks = []target.MySQLForeignKey{
		testOracleMySQLTargetFK("dst", "C", "dst", "P", "old_cp"),
		testOracleMySQLTargetFK("dst", "P", "dst", "C", "old_pc"),
		testOracleMySQLTargetFK("dst", "S", "dst", "S", "old_self"),
	}
	r.fks = []source.FKInfo{
		{TableName: "C", RefSchema: "APP", RefTable: "P", ConstraintName: "new_cp", Columns: []string{"ID"}, RefColumns: []string{"ID"}},
		{TableName: "P", RefSchema: "APP", RefTable: "C", ConstraintName: "new_pc", Columns: []string{"ID"}, RefColumns: []string{"ID"}},
		{TableName: "S", RefSchema: "APP", RefTable: "S", ConstraintName: "new_self", Columns: []string{"ID"}, RefColumns: []string{"ID"}},
	}
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Len(t, report.TargetDependencies, 3)
	require.Len(t, w.foreignKeys, 3)
	for i := 0; i < 3; i++ {
		require.Equal(t, w.fks[i].DropSQL, w.events[i])
	}
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "target_dependency" {
			require.True(t, object.DiagnosticOnly)
			require.False(t, object.CanExecuteDDL())
			require.True(t, object.Units[0].Destructive)
			require.False(t, object.Units[0].Selected)
			require.NotEmpty(t, object.SourceDDL)
		}
	}
}

func TestOracleMySQLExternalAndBlockedChildrenProtectParents(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%v", external), func(t *testing.T) {
			m, r, w := oracleMySQLFixture(t, "P", "C", "I")
			if external {
				w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("other", "retained", "dst", "C", "external"), testOracleMySQLTargetFK("dst", "C", "dst", "P", "cp")}
			} else {
				invalid := "'not a number'"
				r.ddl["C"].Columns[0].Default = &invalid
				w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "C", "dst", "P", "cp")}
			}
			report := m.Run(context.Background())
			require.Equal(t, 2, report.Tables.Failed)
			require.Len(t, w.created, 1)
			require.Contains(t, w.created[0], "`dst`.`I`")
			require.Empty(t, report.TargetDependencies)
			for _, event := range w.events {
				require.NotContains(t, event, "DROP FOREIGN KEY")
			}
		})
	}
}

func TestOracleMySQLCleanupFailureRestoresBeforeAnyTableDrop(t *testing.T) {
	m, _, w := oracleMySQLFixture(t, "P", "C", "S")
	w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "C", "dst", "P", "first"), testOracleMySQLTargetFK("dst", "S", "dst", "S", "second")}
	w.dependencyFail = w.fks[1].DropSQL
	m.cfg.CaptureRepair = true
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Equal(t, []string{w.fks[0].DropSQL, w.fks[1].DropSQL, w.fks[0].RestoreSQL}, w.events)
	require.Equal(t, "succeeded", report.TargetDependencies[0].RestoreStatus)
	require.Equal(t, "failed", report.TargetDependencies[1].DropStatus)
}

func TestOracleMySQLCreateFailureContinuesHealthyEndpoints(t *testing.T) {
	m, _, w := oracleMySQLFixture(t, "P", "C", "S")
	w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "C", "dst", "P", "cp"), testOracleMySQLTargetFK("dst", "S", "dst", "S", "self")}
	w.createFail = "P"
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Len(t, w.created, 2)
	require.Equal(t, 2, report.Tables.Success)
	require.Equal(t, 1, report.Tables.Failed)
	require.NotContains(t, w.events, w.fks[1].RestoreSQL)
	require.NotContains(t, w.events, w.fks[0].RestoreSQL)
	require.Equal(t, "blocked", report.TargetDependencies[0].RestoreStatus)
	require.Equal(t, "not_required", report.TargetDependencies[1].RestoreStatus)
	require.Empty(t, w.foreignKeys)
}

func TestOracleMySQLForeignKeysWaitForDataAndKeys(t *testing.T) {
	for _, failure := range []string{"copy", "key", "excluded_key"} {
		t.Run(failure, func(t *testing.T) {
			m, r, w := oracleMySQLFixture(t, "P", "C")
			r.fks = []source.FKInfo{{TableName: "C", RefTable: "P", RefSchema: "APP", ConstraintName: "cp", Columns: []string{"ID"}, RefColumns: []string{"ID"}}}
			if failure == "copy" {
				m.cfg.Content = "both"
				r.rows["P"], r.rows["C"] = [][]any{{1}}, [][]any{{1}}
				w.copyFail = "P"
			} else if failure == "key" {
				w.indexFail = "P"
			} else {
				m.cfg.ExcludedObjects = []ObjectRef{{Kind: "primary_key", Name: "PRIMARY", Table: "P"}}
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Empty(t, w.foreignKeys)
			require.Equal(t, 1, report.Constraints.Failed)
			require.Equal(t, "dependency", report.Constraints.Items[0].Stage)
		})
	}
}

func TestOracleMySQLCancellationDuringPostDDLRecordsUnrestoredConstraints(t *testing.T) {
	m, _, w := oracleMySQLFixture(t, "P", "C")
	w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "C", "dst", "P", "cp")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.cancelOnIndex = cancel
	m.cfg.CaptureRepair = true
	report := m.Run(ctx)
	require.True(t, report.HasFailures())
	require.Len(t, report.TargetDependencies, 1)
	require.Equal(t, "blocked", report.TargetDependencies[0].RestoreStatus)
	require.NotEmpty(t, report.TargetDependencies[0].Error)
	require.Empty(t, w.foreignKeys)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "target_dependency" {
			require.Equal(t, "blocked", object.Units[1].Status)
			require.NotEmpty(t, object.Units[1].Error)
		}
	}
}

func TestOracleMySQLTargetDriftAndPersistenceBlockAllDDL(t *testing.T) {
	for _, failure := range []string{"drift", "catalog", "save"} {
		t.Run(failure, func(t *testing.T) {
			m, _, w := oracleMySQLFixture(t, "P", "C")
			w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "C", "dst", "P", "cp")}
			switch failure {
			case "drift":
				w.drift = true
			case "catalog":
				w.inspectError = errors.New("catalog denied")
			case "save":
				m.cfg.CaptureRepair = true
				m.cfg.RepairSink = func(RepairManifest) error { return errors.New("disk full") }
			}
			report := m.Run(context.Background())
			require.True(t, report.HasFailures())
			require.Empty(t, w.created)
			require.Empty(t, w.events)
		})
	}
}

func TestOracleMySQLTargetCaseRules(t *testing.T) {
	t.Run("catalog lowercase matches uppercase source", func(t *testing.T) {
		m, _, w := oracleMySQLFixture(t, "P", "C")
		w.caseMode = 1
		w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "c", "dst", "p", "cp")}
		report := m.Run(context.Background())
		require.False(t, report.HasFailures(), "%+v", report)
		require.Equal(t, w.fks[0].DropSQL, w.events[0])
	})
	for _, caseMode := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(caseMode), func(t *testing.T) {
			m, _, w := oracleMySQLFixture(t, "Foo", "foo")
			w.caseMode = caseMode
			report := m.Run(context.Background())
			if caseMode == 0 {
				require.False(t, report.HasFailures(), "%+v", report)
				require.Len(t, w.created, 2)
			} else {
				require.True(t, report.HasFailures())
				require.Empty(t, w.created)
			}
		})
	}
}

func TestOracleMySQLRepairPreparationKeepsFrozenSourceSnapshot(t *testing.T) {
	m, r, _ := oracleMySQLFixture(t, "P", "C", "I")
	m.cfg.CaptureRepair = true
	r.triggerCount = 7
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Equal(t, map[string]int{"P": 1, "C": 1, "I": 1}, r.reads)
	require.Equal(t, 7, report.Triggers.Total)
}

func TestOracleMySQLVersionFailureBlocksWideTableOnly(t *testing.T) {
	m, r, w := oracleMySQLFixture(t, "Wide", "Narrow")
	for i := 0; i < 10; i++ {
		r.ddl["Wide"].Columns = append(r.ddl["Wide"].Columns, source.ColumnInfo{Name: fmt.Sprintf("C%d", i), DataType: "VARCHAR2", Length: 2000, IsNullable: true})
	}
	w.versionError = errors.New("version probe denied")
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Len(t, w.created, 1)
	require.Contains(t, w.created[0], "`Narrow`")
	require.Contains(t, report.Tables.Items[0].Error, "version probe denied")
}

func TestOracleMySQLDataAndObjectModesDoNotCleanTargetDependencies(t *testing.T) {
	for _, mode := range []string{"data", "objects"} {
		t.Run(mode, func(t *testing.T) {
			m, _, w := oracleMySQLFixture(t, "P", "C")
			w.fks = []target.MySQLForeignKey{testOracleMySQLTargetFK("dst", "C", "dst", "P", "cp")}
			if mode == "data" {
				m.cfg.Content = "data_only"
			} else {
				m.cfg.Objects = []string{"indexes"}
			}
			m.Run(context.Background())
			require.Zero(t, w.inspectCalls)
			for _, query := range w.events {
				require.NotContains(t, query, "DROP FOREIGN KEY")
			}
		})
	}
}

type oracleMySQLPartitionTestReader struct {
	*oracleMySQLTestReader
	spec *source.PartitionSpec
}

func (r *oracleMySQLPartitionTestReader) PartitionRoots(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *oracleMySQLPartitionTestReader) PartitionMetadata(context.Context, string) (*source.PartitionSpec, error) {
	return source.ClonePartitionSpec(r.spec), nil
}
func (r *oracleMySQLPartitionTestReader) StreamPartitionRows(context.Context, string, int, func([]string, []string, [][]any) error) error {
	return errors.New("schema-only test must not stream data")
}

type oracleMySQLPartitionTestWriter struct {
	*oracleMySQLTestWriter
	m *Migrator
}

func (w *oracleMySQLPartitionTestWriter) InspectPartitionProfile(context.Context) (partition.Profile, error) {
	return partition.Profile{Product: "mysql", Version: "8.0.30", Schema: "dst", Features: map[string]bool{"inline_mysql": true, partition.FeatureRange: true, partition.FeatureMultiRange: true}}, nil
}
func (w *oracleMySQLPartitionTestWriter) InspectPartitionTable(_ context.Context, table string) (*source.PartitionSpec, error) {
	return source.ClonePartitionSpec(w.m.partitionState.tables[table].Plan.Target), nil
}
func (w *oracleMySQLPartitionTestWriter) CountPartitionRows(context.Context, string, *source.PartitionSpec) (map[string]int64, error) {
	return map[string]int64{}, nil
}
func (w *oracleMySQLPartitionTestWriter) ValidatePartitionNames(context.Context, string, *source.PartitionSpec) error {
	return nil
}
func (w *oracleMySQLPartitionTestWriter) VerifyPartitionKeyColumns(context.Context, string, *partition.Plan) error {
	return nil
}

func TestOracleMySQLPartitionDDLUsesSameWideTablePlan(t *testing.T) {
	m, baseReader, baseWriter := oracleMySQLFixture(t, "Wide")
	for i := 0; i < 10; i++ {
		baseReader.ddl["Wide"].Columns = append(baseReader.ddl["Wide"].Columns, source.ColumnInfo{Name: fmt.Sprintf("C%d", i), DataType: "VARCHAR2", Length: 2000, IsNullable: true})
	}
	spec := partitionFlowSpec()
	spec.Method = "RANGE"
	spec.Keys = []source.PartitionKey{{Column: "ID", DataType: "NUMBER"}}
	r := &oracleMySQLPartitionTestReader{oracleMySQLTestReader: baseReader, spec: spec}
	w := &oracleMySQLPartitionTestWriter{oracleMySQLTestWriter: baseWriter, m: m}
	m.reader, m.writer, m.cfg.CaptureRepair = r, w, true
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Len(t, w.created, 1)
	require.Contains(t, w.created[0], "longtext")
	require.Contains(t, w.created[0], "PARTITION BY")
	require.Contains(t, w.created[0], "`ID` int NOT NULL")
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "table" {
			require.Equal(t, w.created[0], object.DDL)
			require.Equal(t, "longtext", object.Columns[1].DataType)
			require.Equal(t, "VARCHAR2", object.SourceColumns[1].DataType)
		}
	}
}

func TestOracleMySQLTrailingSpaceTablePreflightSkipsOnlyInvalidName(t *testing.T) {
	m, _, w := oracleMySQLFixture(t, "Bad ", "Good")
	m.cfg.Mode = "all"
	m.cfg.Filter = ""
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Equal(t, 1, report.Tables.Success)
	require.NotContains(t, strings.Join(w.events, "\n"), "`Bad `")
	require.Contains(t, strings.Join(w.events, "\n"), "`Good`")
}
