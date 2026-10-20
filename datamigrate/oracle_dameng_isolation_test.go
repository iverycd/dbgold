package datamigrate

import (
	"context"
	"database/sql/driver"
	"fmt"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type oracleDMFaultWriter struct {
	*oracleDMWriter
	prepareErrors, existingErrors, copyErrors map[string]error
	createError, loadError                    error
	preparedTables, attemptedCopies           []string
	legacyLoadCalls                           int
}

type oracleDMFaultReader struct {
	*oracleDMReader
	metadataErrors, sourceErrors map[string]error
	sourceDDLError               error
}

func (r *oracleDMFaultReader) MetadataReadConcurrency() int { return 2 }

func (r *oracleDMFaultReader) GetTableSourceDDL(context.Context, string) (string, error) {
	return "", r.sourceDDLError
}

func (r *oracleDMFaultReader) GetTableDDLInfo(ctx context.Context, table string) (*source.TableDDLInfo, error) {
	if err := r.metadataErrors[table]; err != nil {
		return nil, err
	}
	return r.oracleDMReader.GetTableDDLInfo(ctx, table)
}

func (r *oracleDMFaultReader) ValidateOracleDamengSource(_ context.Context, infos []*source.TableDDLInfo) error {
	for _, info := range infos {
		if err := r.sourceErrors[info.TableName]; err != nil {
			return err
		}
	}
	return nil
}

func (w *oracleDMFaultWriter) PrepareOracleDameng(_ context.Context, infos []*source.TableDDLInfo, _ dialect.TypeOpt) error {
	for _, info := range infos {
		w.preparedTables = append(w.preparedTables, info.TableName)
		if err := w.prepareErrors[info.TableName]; err != nil {
			return err
		}
	}
	return nil
}
func (w *oracleDMFaultWriter) ValidateOracleDamengExisting(_ context.Context, info *source.TableDDLInfo, _ dialect.TypeOpt) error {
	return w.existingErrors[info.TableName]
}
func (w *oracleDMFaultWriter) PrepareOracleDamengLoad(context.Context) error {
	w.legacyLoadCalls++
	return w.loadError
}
func (w *oracleDMFaultWriter) CreateTable(ctx context.Context, ss []dialect.Statement) error {
	if w.createError != nil {
		return w.createError
	}
	return w.oracleDMWriter.CreateTable(ctx, ss)
}
func (w *oracleDMFaultWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]interface{}) error {
	w.attemptedCopies = append(w.attemptedCopies, table)
	if err := w.copyErrors[table]; err != nil {
		return err
	}
	return w.oracleDMWriter.CopyData(ctx, table, cols, types, rows)
}

func oracleDMIsolationFixture(mode string) (*oracleDMReader, *oracleDMFaultWriter, *Migrator) {
	r, w, m := oracleDMFixture(mode)
	r.tables = append(r.tables, "Later")
	info := *r.ddl["Mixed"]
	info.TableName = "Later"
	info.Columns = append([]source.ColumnInfo(nil), info.Columns...)
	r.ddl["Later"] = &info
	faults := &oracleDMFaultWriter{oracleDMWriter: w}
	m.writer = faults
	return r, faults, m
}

func TestOracleDamengPerTableCapabilitiesContinueAllModes(t *testing.T) {
	for _, mode := range []string{"schema_only", "both", "data_only"} {
		t.Run(mode, func(t *testing.T) {
			_, w, m := oracleDMIsolationFixture(mode)
			w.prepareErrors = map[string]error{"Mixed": &target.OracleDamengCapabilityError{Scope: "column", Stage: "国家字符绑定校验失败", Err: fmt.Errorf("Mixed.Txt: NVARCHAR 字符能力不足")}}
			report := m.Run(context.Background())
			require.Equal(t, 2, report.Tables.Total)
			require.Equal(t, 1, report.Tables.Failed)
			require.Equal(t, "Mixed", report.Tables.Items[0].Name)
			require.Equal(t, "preflight", report.Tables.Items[0].Stage)
			require.True(t, m.dmBlocked["Mixed"])
			require.False(t, m.dmBlocked["Later"])
			require.Equal(t, []string{"Mixed", "Later"}, w.preparedTables)
			if mode == "data_only" {
				require.Empty(t, w.created)
			} else {
				require.Equal(t, 1, report.Tables.Success)
				require.Len(t, w.created, 1)
				require.Contains(t, w.created[0], `"LATER"`)
			}
			if mode != "schema_only" {
				require.Equal(t, []string{"LATER"}, w.attemptedCopies)
				require.Equal(t, 1, report.Data.Success)
				require.Len(t, report.RowCounts, 1)
				require.Equal(t, "LATER", report.RowCounts[0].Table)
			}
		})
	}
}

func TestOracleDamengExistingCapacityFailureOnlyBlocksOneTable(t *testing.T) {
	_, w, m := oracleDMIsolationFixture("data_only")
	w.existingErrors = map[string]error{"Mixed": fmt.Errorf("目标列容量不足")}
	w.loadError = fmt.Errorf("宽松模式的旧 CAST 负例不能成为任务门槛")
	report := m.Run(context.Background())
	require.Equal(t, 1, report.Tables.Failed)
	require.Equal(t, []string{"LATER"}, w.attemptedCopies)
	require.Equal(t, 1, report.Data.Success)
	require.Zero(t, w.legacyLoadCalls)
}

func TestOracleDamengSourcePreflightFailureIsIsolated(t *testing.T) {
	for _, stage := range []string{"metadata", "source"} {
		t.Run(stage, func(t *testing.T) {
			r, w, m := oracleDMIsolationFixture("both")
			reader := &oracleDMFaultReader{oracleDMReader: r}
			if stage == "metadata" {
				reader.metadataErrors = map[string]error{"Mixed": fmt.Errorf("该表列元数据不可见")}
			} else {
				reader.sourceErrors = map[string]error{"Mixed": fmt.Errorf("该表 XML 投影不支持")}
			}
			m.reader = reader
			report := m.Run(context.Background())
			require.Equal(t, 1, report.Tables.Failed)
			require.Equal(t, "Mixed", report.Tables.Items[0].Name)
			require.Len(t, w.created, 1)
			require.Contains(t, w.created[0], `"LATER"`)
			require.Equal(t, []string{"LATER"}, w.attemptedCopies)
		})
	}
}

func TestOracleDamengSourceConnectionFailureStopsMetadataAndWrites(t *testing.T) {
	r, w, m := oracleDMIsolationFixture("both")
	m.reader = &oracleDMFaultReader{oracleDMReader: r, metadataErrors: map[string]error{"Mixed": fmt.Errorf("读取列: %w", driver.ErrBadConn)}}
	report := m.Run(context.Background())
	require.Empty(t, w.preparedTables)
	require.Empty(t, w.created)
	require.Empty(t, w.attemptedCopies)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
}

func TestOracleDamengTaskErrorDuringRepairPreparationIsNotPersistenceError(t *testing.T) {
	_, w, m := oracleDMIsolationFixture("both")
	w.prepareErrors = map[string]error{"Mixed": driver.ErrBadConn}
	m.cfg.CaptureRepair = true
	m.cfg.RepairSink = func(RepairManifest) error { return nil }
	report := m.Run(context.Background())
	require.Empty(t, w.created)
	require.Empty(t, w.attemptedCopies)
	require.Len(t, report.Tables.Items, 1)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
	require.NotContains(t, report.Tables.Items[0].Name, "持久化")
}

func TestOracleDamengConnectionLossDuringMetadataPrewarmStopsTask(t *testing.T) {
	r, w, m := oracleDMIsolationFixture("both")
	m.reader = &oracleDMFaultReader{oracleDMReader: r, metadataErrors: map[string]error{"Mixed": driver.ErrBadConn}}
	m.cfg.CaptureRepair = true
	m.cfg.RepairSink = func(RepairManifest) error { return nil }
	report := m.Run(context.Background())
	require.Empty(t, w.created)
	require.Empty(t, w.attemptedCopies)
	require.Len(t, report.Tables.Items, 1)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
	require.Contains(t, report.Tables.Items[0].Error, driver.ErrBadConn.Error())
}

func TestOracleDamengCancelledTaskDoesNotReportPersistenceFailure(t *testing.T) {
	_, w, m := oracleDMIsolationFixture("both")
	m.cfg.CaptureRepair = true
	m.cfg.RepairSink = func(RepairManifest) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := m.Run(ctx)
	require.Empty(t, w.created)
	require.Empty(t, w.attemptedCopies)
	require.Empty(t, report.Data.Items)
	require.Len(t, report.Tables.Items, 1)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
	require.NotContains(t, report.Tables.Items[0].Name, "持久化")
}

func TestOracleDamengConnectionErrorSurvivesRepairCheckpointError(t *testing.T) {
	_, w, m := oracleDMIsolationFixture("schema_only")
	w.createError = driver.ErrBadConn
	m.cfg.CaptureRepair = true
	m.cfg.RepairSink = func(RepairManifest) error { return nil }
	updates := 0
	m.cfg.RepairObjectSink = func(repair.Object) error {
		updates++
		if updates == 2 {
			return fmt.Errorf("修复记录保存失败")
		}
		return nil
	}
	report := m.Run(context.Background())
	require.Empty(t, w.created)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
	require.Contains(t, report.Tables.Items[0].Error, driver.ErrBadConn.Error())
	require.Contains(t, report.Tables.Items[0].Error, "修复记录保存失败")
}

func TestOracleDamengConnectionLossDuringIndexDiagnosticStopsPostDDL(t *testing.T) {
	r, w, m := oracleDMIsolationFixture("schema_only")
	r.indexes = []source.IndexInfo{
		{TableName: "Mixed", IndexName: "Unsupported", Unsupported: "不支持此表达式索引"},
		{TableName: "Later", IndexName: "LaterIndex", Columns: []string{"Txt"}},
	}
	m.reader = &oracleDMFaultReader{oracleDMReader: r, sourceDDLError: driver.ErrBadConn}
	report := m.Run(context.Background())
	require.Len(t, w.created, 2)
	require.Empty(t, w.indexes, "诊断读取失去连接后，不得继续后面的索引")
	require.Len(t, report.Indexes.Items, 1, "已发生的索引转换失败仍需保留详情")
	require.Empty(t, w.views)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
}

func TestOracleDamengTaskErrorsStopBeforeBusinessWrites(t *testing.T) {
	for _, stage := range []string{"preflight", "create", "string-control"} {
		t.Run(stage, func(t *testing.T) {
			_, w, m := oracleDMIsolationFixture("both")
			switch stage {
			case "preflight":
				w.prepareErrors = map[string]error{"Mixed": fmt.Errorf("连接检查: %w", driver.ErrBadConn)}
			case "create":
				w.createError = fmt.Errorf("建表连接已断开: %w", driver.ErrBadConn)
			case "string-control":
				w.prepareErrors = map[string]error{"Mixed": &target.OracleDamengCapabilityError{Scope: "task", Stage: "普通字符串绑定控制失败", Err: fmt.Errorf("数据库侧长度/字节校验未通过")}}
			}
			report := m.Run(context.Background())
			require.Empty(t, w.created)
			require.Empty(t, w.attemptedCopies)
			require.Empty(t, w.indexes)
			require.Equal(t, 1, report.Tables.Failed)
			require.Equal(t, "task", report.Tables.Items[0].Stage)
			require.Contains(t, report.Tables.Items[0].Error, "迁移任务已终止")
			if stage == "preflight" || stage == "string-control" {
				require.Equal(t, []string{"Mixed"}, w.preparedTables)
			}
		})
	}
}

func TestOracleDamengDataFailureContinuesButConnectionFailureStopsDispatch(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint(fatal), func(t *testing.T) {
			_, w, m := oracleDMIsolationFixture("both")
			err := fmt.Errorf("该页有非法值")
			if fatal {
				err = fmt.Errorf("连接中断: %w", driver.ErrBadConn)
			}
			w.copyErrors = map[string]error{"MIXED": err}
			report := m.Run(context.Background())
			if fatal {
				require.Equal(t, []string{"MIXED"}, w.attemptedCopies)
				require.Empty(t, report.RowCounts)
				require.Equal(t, "task", report.Tables.Items[0].Stage)
			} else {
				require.Equal(t, []string{"MIXED", "LATER"}, w.attemptedCopies)
				require.Equal(t, 1, report.Data.Failed)
				require.Equal(t, 1, report.Data.Success)
				require.True(t, m.dmBlocked["Mixed"])
				require.Len(t, report.RowCounts, 1)
				require.Equal(t, "LATER", report.RowCounts[0].Table)
			}
		})
	}
}
