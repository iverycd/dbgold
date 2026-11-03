package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/partition"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

type autoMigrationReader struct {
	*oracleMySQLTestReader
	bindings []source.OracleMySQLAutoIncrement
	badTable string
}

func (r *autoMigrationReader) GetOracleMySQLAutoIncrements(context.Context) ([]source.OracleMySQLAutoIncrement, error) {
	return r.bindings, nil
}
func (r *autoMigrationReader) GetSequences(context.Context) ([]source.SequenceInfo, error) {
	var out []source.SequenceInfo
	for _, a := range r.bindings {
		if !a.Disabled {
			copy := a
			out = append(out, source.SequenceInfo{TableName: a.TableName, ColumnName: a.ColumnName, StartValueText: a.StartValueText, OracleMySQL: &copy, Unsupported: a.Error})
		}
	}
	return out, nil
}
func (r *autoMigrationReader) GetTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	if table == r.badTable {
		return nil, errors.New("自增列含小数")
	}
	return r.oracleMySQLTestReader.GetTableDDLInfo(ctx, table)
}

type autoMigrationWriter struct {
	*oracleMySQLTestWriter
	calibrated      []string
	calibrationFail bool
	validateFail    bool
}

func (w *autoMigrationWriter) ValidateOracleMySQLAutoIncrement(context.Context, source.SequenceInfo, string, bool) error {
	if w.validateFail {
		return errors.New("目标列不是自增列")
	}
	return nil
}
func (w *autoMigrationWriter) PrepareOracleMySQLAutoIncrement(_ context.Context, s source.SequenceInfo) ([]dialect.Statement, string, error) {
	return []dialect.Statement{{SQL: "ALTER TABLE `dst`.`" + s.TableName + "` AUTO_INCREMENT = 1000"}}, "1000", nil
}
func (w *autoMigrationWriter) ExecuteOracleMySQLAutoIncrement(ctx context.Context, s source.SequenceInfo, ss []dialect.Statement, start string) error {
	w.calibrated = append(w.calibrated, s.TableName)
	if err := repair.Observe(ctx, ss[0].SQL, "running", nil); err != nil {
		return err
	}
	if w.calibrationFail {
		err := errors.New("校准失败")
		return errors.Join(err, repair.Observe(ctx, ss[0].SQL, "failed", err))
	}
	return repair.Observe(ctx, ss[0].SQL, "succeeded", nil)
}
func autoMigrationFixture(t *testing.T, names ...string) (*Migrator, *autoMigrationReader, *autoMigrationWriter) {
	m, r, w := oracleMySQLFixture(t, names...)
	ar := &autoMigrationReader{oracleMySQLTestReader: r}
	for _, name := range names {
		r.ddl[name].Columns[0].Extra = "auto_increment"
		r.ddl[name].Columns[0].OracleMySQLAutoType = "int"
		ar.bindings = append(ar.bindings, source.OracleMySQLAutoIncrement{TableName: name, ColumnName: "ID", TriggerName: "TG_" + name, SequenceOwner: "APP", SequenceName: "HIBERNATE_SEQUENCE", StartValueText: "800", SharedSequence: len(names) > 1})
		r.rows[name] = [][]any{{"0"}, {"100"}}
	}
	aw := &autoMigrationWriter{oracleMySQLTestWriter: w}
	m = NewMigrator(ar, aw, &Job{LogCh: make(chan string, 4096)}, m.cfg)
	m.cfg.CaptureRepair = true
	return m, ar, aw
}

func TestOracleMySQLAutoMigrationModesAndReport(t *testing.T) {
	for _, mode := range []string{"both", "schema_only", "data_only", "objects"} {
		t.Run(mode, func(t *testing.T) {
			m, _, w := autoMigrationFixture(t, "TableA", "TableB")
			m.cfg.Content = mode
			m.cfg.LowerCaseNames = true
			if mode == "objects" {
				m.cfg.Content = "schema_only"
				m.cfg.Objects = []string{"sequences"}
			}
			report := m.Run(context.Background())
			require.False(t, report.HasFailures(), "%+v", report)
			require.ElementsMatch(t, []string{"tablea", "tableb"}, w.calibrated)
			require.Equal(t, 2, report.TriggerAdaptation.Success)
			for _, item := range report.TriggerAdaptation.Items {
				require.Equal(t, "1000", item.Start)
				require.True(t, item.SharedSequence)
				require.Equal(t, "succeeded", item.CalibrationStatus)
				require.Equal(t, strings.ToLower(item.Table), item.TargetTable)
			}
			if mode == "data_only" || mode == "objects" {
				require.Empty(t, w.created)
			} else {
				require.Len(t, w.created, 2)
				require.Contains(t, w.created[0], "AUTO_INCREMENT")
				require.Contains(t, w.created[0], "KEY `__migration_auto`")
			}
			for _, obj := range report.RepairManifest.Objects {
				if obj.Kind == "sequence" {
					require.Equal(t, "succeeded", obj.Status)
					require.Len(t, obj.Units, 1)
					require.Contains(t, obj.Units[0].SQL, "1000")
					require.NotNil(t, obj.Sequence.OracleMySQL)
				}
			}
		})
	}
}

func TestOracleMySQLAutoBadTableNeverDropped(t *testing.T) {
	m, r, w := autoMigrationFixture(t, "Bad", "Good")
	r.badTable = "Bad"
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Equal(t, 1, report.Tables.Success)
	require.Len(t, w.created, 1)
	require.NotContains(t, strings.Join(w.events, "\n"), "DROP TABLE IF EXISTS `dst`.`Bad`")
	require.Equal(t, []string{"Good"}, w.calibrated)
	require.Equal(t, 1, report.TriggerAdaptation.Blocked)
	require.Equal(t, 1, report.TriggerAdaptation.Success)
}

func TestOracleMySQLAutoFailedCalibrationNotSuccessfulConversion(t *testing.T) {
	m, _, w := autoMigrationFixture(t, "T")
	w.calibrationFail = true
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Sequences.Failed)
	require.Equal(t, 1, report.TriggerAdaptation.Failed)
	require.Equal(t, "converted", report.TriggerAdaptation.Items[0].ConversionStatus)
	require.Equal(t, "failed", report.TriggerAdaptation.Items[0].CalibrationStatus)
	require.Contains(t, report.Sequences.Items[0].DDL, "AUTO_INCREMENT = 1000")
	for _, obj := range report.RepairManifest.Objects {
		if obj.Kind == "sequence" {
			require.Equal(t, "failed", obj.Status)
			require.Equal(t, "failed", obj.Units[0].Status)
		}
	}
}

func TestOracleMySQLAutoDataOnlyCountMismatchBlocksCalibration(t *testing.T) {
	m, _, w := autoMigrationFixture(t, "T")
	m.cfg.Content = "data_only"
	w.counts["T"] = 1
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.calibrated)
	require.Equal(t, 1, report.TriggerAdaptation.Blocked)
}

func TestOracleMySQLAutoDataOnlyRejectsMissingBindingBeforeCopy(t *testing.T) {
	m, _, w := autoMigrationFixture(t, "T")
	m.cfg.Content = "data_only"
	w.validateFail = true
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.counts)
	require.Empty(t, w.calibrated)
	require.Empty(t, w.created)
}

func TestOracleMySQLAutoCopyFailureBlocksCalibration(t *testing.T) {
	m, _, w := autoMigrationFixture(t, "T")
	m.cfg.Content = "both"
	w.copyFail = "T"
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.calibrated)
	require.Equal(t, "converted", report.TriggerAdaptation.Items[0].ConversionStatus)
	require.Equal(t, "blocked", report.TriggerAdaptation.Items[0].CalibrationStatus)
}

func TestOracleMySQLAutoForeignKeyTypeChangeBlocksBothTables(t *testing.T) {
	m, r, w := autoMigrationFixture(t, "P", "C")
	r.ddl["P"].Columns[0].OracleMySQLAutoType = "bigint"
	r.fks = []source.FKInfo{{TableName: "C", Columns: []string{"ID"}, RefTable: "P", RefColumns: []string{"ID"}, ConstraintName: "FK_C_P"}}
	report := m.Run(context.Background())
	require.True(t, report.HasFailures())
	require.Empty(t, w.created)
	require.Empty(t, w.calibrated)
	require.Equal(t, 2, report.TriggerAdaptation.Blocked)
}

type autoPartitionReader struct {
	*autoMigrationReader
	spec *source.PartitionSpec
}

func (r *autoPartitionReader) PartitionRoots(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *autoPartitionReader) PartitionMetadata(context.Context, string) (*source.PartitionSpec, error) {
	return source.ClonePartitionSpec(r.spec), nil
}
func (r *autoPartitionReader) StreamPartitionRows(context.Context, string, int, func([]string, []string, [][]any) error) error {
	return errors.New("schema-only must not read rows")
}

type autoPartitionWriter struct {
	*autoMigrationWriter
	inspector *oracleMySQLPartitionTestWriter
}

func (w *autoPartitionWriter) InspectPartitionProfile(ctx context.Context) (partition.Profile, error) {
	return w.inspector.InspectPartitionProfile(ctx)
}
func (w *autoPartitionWriter) InspectPartitionTable(ctx context.Context, table string) (*source.PartitionSpec, error) {
	return w.inspector.InspectPartitionTable(ctx, table)
}
func (w *autoPartitionWriter) CountPartitionRows(ctx context.Context, table string, s *source.PartitionSpec) (map[string]int64, error) {
	return w.inspector.CountPartitionRows(ctx, table, s)
}
func (w *autoPartitionWriter) ValidatePartitionNames(ctx context.Context, table string, s *source.PartitionSpec) error {
	return w.inspector.ValidatePartitionNames(ctx, table, s)
}
func (w *autoPartitionWriter) VerifyPartitionKeyColumns(ctx context.Context, table string, p *partition.Plan) error {
	return w.inspector.VerifyPartitionKeyColumns(ctx, table, p)
}

func TestOracleMySQLAutoPartitionPlanKeepsAutoColumnAndRepairBinding(t *testing.T) {
	m, r, w := autoMigrationFixture(t, "T")
	spec := partitionFlowSpec()
	spec.Method = "RANGE"
	spec.Keys = []source.PartitionKey{{Column: "ID", DataType: "NUMBER"}}
	m.reader = &autoPartitionReader{autoMigrationReader: r, spec: spec}
	m.writer = &autoPartitionWriter{autoMigrationWriter: w, inspector: &oracleMySQLPartitionTestWriter{oracleMySQLTestWriter: w.oracleMySQLTestWriter, m: m}}
	report := m.Run(context.Background())
	require.False(t, report.HasFailures(), "%+v", report)
	require.Len(t, w.created, 1)
	require.Contains(t, w.created[0], "`ID` int NOT NULL AUTO_INCREMENT")
	require.Contains(t, w.created[0], "PARTITION BY")
	require.Equal(t, 1, report.TriggerAdaptation.Success)
	for _, object := range report.RepairManifest.Objects {
		if object.Kind == "table" {
			require.Equal(t, w.created[0], object.DDL)
			require.Equal(t, "int", object.Columns[0].OracleMySQLAutoType)
		}
	}
}
