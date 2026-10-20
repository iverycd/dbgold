package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengHealthyTablesIgnoreLegacyNegativeCastGate(t *testing.T) {
	for _, compatible := range []string{"0", "4", "7"} {
		for _, content := range []string{"schema_only", "both", "data_only"} {
			for _, capture := range []bool{false, true} {
				t.Run(fmt.Sprintf("mode%s/%s/repair=%t", compatible, content, capture), func(t *testing.T) {
					r, w, m := oracleDMIsolationFixture(content)
					for _, table := range r.tables {
						r.ddl[table].Columns[1].DataType = "NVARCHAR2"
					}
					w.loadError = fmt.Errorf("COMPATIBLE_MODE=%s: CAST 非法数值返回 0，旧负例探针不可用于列写入验收", compatible)
					m.cfg.CaptureRepair = capture
					m.cfg.RepairSink = func(RepairManifest) error { return nil }
					report := m.Run(context.Background())
					require.False(t, report.HasFailures(), "%+v", report)
					require.Zero(t, w.legacyLoadCalls, "上层流程不能再用无效的 SELECT CAST 负例作为整任务门槛")
					require.Equal(t, 2, report.Tables.Total)
					if content == "data_only" {
						require.Empty(t, w.created)
					} else {
						require.Len(t, w.created, 2)
						require.Contains(t, w.created[0], `NVARCHAR(100 CHAR)`)
					}
					if content == "schema_only" {
						require.Empty(t, w.attemptedCopies)
					} else {
						require.Equal(t, []string{"MIXED", "LATER"}, w.attemptedCopies)
						require.Equal(t, 2, report.Data.Success)
					}
				})
			}
		}
	}
}

func TestOracleDamengRecoveryPreviewIgnoresLegacyNegativeCastGate(t *testing.T) {
	r, w, m := oracleDMIsolationFixture("both")
	w.loadError = fmt.Errorf("旧严格 CAST 探针被宽松模式转换成 0")
	object := repair.Object{ID: "table-id", Kind: "table", SourceTable: "Mixed", Table: "MIXED", TargetName: "MIXED", SourceColumns: r.ddl["Mixed"].Columns, ColumnMapping: map[string]string{"RowId": "ROW_ID", "Txt": "TXT"}, DataStatus: "pending"}
	ops := recoveryOps{schema: func(context.Context, target.Writer) (string, error) { return "APP", nil }, inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
		return []source.ColumnInfo{{Name: "ROW_ID", DataType: "NUMBER"}, {Name: "TXT", DataType: "VARCHAR", IsNullable: true}}, nil
	}}
	preview, err := previewRecovery(context.Background(), r, w, m.cfg, object, nil, nil, ops)
	require.NoError(t, err)
	require.True(t, preview.DataRequired)
	require.Zero(t, w.legacyLoadCalls)
	require.Empty(t, w.created)
	require.Empty(t, w.attemptedCopies)
}

func TestOracleDamengRealConnectionFailureStillStopsBeforeWrites(t *testing.T) {
	_, w, m := oracleDMIsolationFixture("both")
	w.loadError = fmt.Errorf("不应运行的旧 CAST 探针")
	w.prepareErrors = map[string]error{"Mixed": fmt.Errorf("目标元数据读取失败: %w", &dm.DmError{ErrCode: 6001, ErrText: "communication error"})}
	report := m.Run(context.Background())
	require.Empty(t, w.created)
	require.Empty(t, w.attemptedCopies)
	require.Zero(t, w.legacyLoadCalls)
	require.Len(t, report.Tables.Items, 1)
	require.Equal(t, "task", report.Tables.Items[0].Stage)
	require.Contains(t, report.Tables.Items[0].Error, "communication error")
}

type oracleDMCapacityPageReader struct {
	*oracleDMReader
	all   map[string][][]any
	reads map[string][]int64
}

func (r *oracleDMCapacityPageReader) ReadPage(_ context.Context, table string, _ []string, offset, limit int64) ([]string, []string, [][]any, error) {
	r.reads[table] = append(r.reads[table], offset)
	rows := r.all[table]
	end := min(offset+limit, int64(len(rows)))
	if offset > end {
		offset = end
	}
	return []string{"RowId", "Txt"}, []string{"NUMBER", "NVARCHAR2"}, rows[offset:end], nil
}

func (r *oracleDMCapacityPageReader) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(r.all[table])), nil
}

type oracleDMCapacityPageWriter struct {
	*oracleDMFaultWriter
	committed, attempted map[string][]string
}

func (w *oracleDMCapacityPageWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	for _, row := range rows {
		id := row[0].(string)
		w.attempted[table] = append(w.attempted[table], id)
		if table == "MIXED" && id == "2" {
			return fmt.Errorf("目标列 MIXED.TXT 实际字符容量为 2，当前值超出容量")
		}
	}
	if err := w.oracleDMFaultWriter.CopyData(ctx, table, cols, types, rows); err != nil {
		return err
	}
	for _, row := range rows {
		w.committed[table] = append(w.committed[table], row[0].(string))
	}
	return nil
}

func (w *oracleDMCapacityPageWriter) CountRows(_ context.Context, table string) (int64, error) {
	return int64(len(w.committed[strings.ToUpper(table)])), nil
}

func TestOracleDamengActualPageCapacityFailureKeepsPriorPagesAndOtherTables(t *testing.T) {
	r, faults, m := oracleDMIsolationFixture("both")
	for _, table := range r.tables {
		r.ddl[table].Columns[1].DataType = "NVARCHAR2"
	}
	reader := &oracleDMCapacityPageReader{oracleDMReader: r, reads: map[string][]int64{}, all: map[string][][]any{
		"Mixed": {{"1", "汉A"}, {"2", "超出实际容量"}, {"3", "不再读取"}},
		"Later": {{"4", "汉B"}},
	}}
	w := &oracleDMCapacityPageWriter{oracleDMFaultWriter: faults, committed: map[string][]string{}, attempted: map[string][]string{}}
	m.reader, m.writer, m.cfg.PageSize = reader, w, 1
	report := m.Run(context.Background())
	require.Zero(t, report.Tables.Failed, "DDL 与真实写入容量检查的结果应分开记录")
	require.Equal(t, 1, report.Data.Failed)
	require.Equal(t, 1, report.Data.Success)
	require.Equal(t, []string{"1", "2"}, w.attempted["MIXED"])
	require.Equal(t, []string{"1"}, w.committed["MIXED"], "当前失败分页不能影响已提交分页")
	require.Equal(t, []string{"4"}, w.committed["LATER"], "其他健康表继续迁移")
	require.Equal(t, []int64{0, 1}, reader.reads["Mixed"], "当前表失败后不再读取后续页")
	require.True(t, m.dmBlocked["Mixed"])
	require.False(t, m.dmBlocked["Later"])
	require.Len(t, report.RowCounts, 1)
	require.Equal(t, "LATER", report.RowCounts[0].Table)
	require.True(t, report.RowCounts[0].Match)
	require.Contains(t, report.Data.Items[0].Error, "实际字符容量")
}
