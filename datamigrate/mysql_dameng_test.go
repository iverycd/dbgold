package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

type mysqlDMCommentWriter struct {
	*dmFixtureWriter
	comments []source.CommentInfo
}

func (w *mysqlDMCommentWriter) CopyData(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	if table == "Orders" && len(rows) >= 41 {
		return fmt.Errorf("表 %s 页内第 41 行: Error -2665: 记录超长", table)
	}
	return w.dmFixtureWriter.CopyData(ctx, table, cols, types, rows)
}

func (w *mysqlDMCommentWriter) CreateComment(ctx context.Context, comment source.CommentInfo) error {
	w.comments = append(w.comments, comment)
	return observeIncrementalRepairSQL(ctx, w.Dialect().CommentStatements("APP", comment), nil)
}

func TestMySQLDamengCommentsAfterDataWriteFailure(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%t", capture), func(t *testing.T) {
			r := newOscarFixture()
			for i := 2; i <= 41; i++ {
				r.rows["Orders"] = append(r.rows["Orders"], []any{i})
			}
			r.comments = []source.CommentInfo{
				{TableName: "Orders", Comment: "订单"},
				{TableName: "Orders", ColumnName: "id", Comment: "编号"},
				{TableName: "Items", ColumnName: "id", Comment: "正常表"},
				{TableName: "Excluded", Comment: "范围外"},
			}
			w := &mysqlDMCommentWriter{dmFixtureWriter: &dmFixtureWriter{mockWriter: &mockWriter{}}}
			m, _ := newTestMigrator(r, w)
			m.cfg.Content, m.cfg.TargetSchema, m.cfg.CaptureRepair = "both", "APP", capture
			m.cfg.PageSize, m.cfg.MaxParallel = 100, 1
			report := m.Run(t.Context())
			require.True(t, report.HasFailures())
			require.Equal(t, 2, report.Tables.Success)
			require.Equal(t, 1, report.Data.Failed)
			require.Equal(t, 1, report.Data.Success)
			require.Equal(t, []string{"Items"}, w.copied)
			require.Contains(t, report.Data.Items[0].Error, "第 1 页写入失败")
			require.Contains(t, report.Data.Items[0].Error, "页内第 41 行: Error -2665: 记录超长")
			require.Equal(t, 3, report.Comments.Total)
			require.Equal(t, 3, report.Comments.Success)
			require.Zero(t, report.Comments.Failed)
			require.Equal(t, []source.CommentInfo{
				{TableName: "Orders", Comment: "订单"},
				{TableName: "Orders", ColumnName: "ID", Comment: "编号"},
				{TableName: "Items", ColumnName: "ID", Comment: "正常表"},
			}, w.comments)
			require.Equal(t, 1, report.PrimaryKeys.Failed)
			if !capture {
				return
			}
			var table repair.Object
			var comments []repair.Object
			for _, object := range report.RepairManifest.Objects {
				if object.SourceTable != "Orders" {
					continue
				}
				switch object.Kind {
				case "table":
					table = object
				case "comment":
					require.Equal(t, "succeeded", object.Status)
					require.Len(t, object.Units, 1)
					require.Equal(t, "succeeded", object.Units[0].Status)
					comments = append(comments, object)
				}
			}
			require.Equal(t, "succeeded", table.Status)
			require.Equal(t, "failed", table.DataStatus)
			require.Len(t, comments, 2)
			ops := recoveryOps{
				schema: func(context.Context, target.Writer) (string, error) { return "APP", nil },
				inspect: func(context.Context, target.Writer, string) ([]source.ColumnInfo, error) {
					return []source.ColumnInfo{{Name: "ID", DataType: "INT", IsNullable: true}}, nil
				},
			}
			preview, err := previewRecovery(t.Context(), r, w, m.cfg, table, comments, nil, ops)
			require.NoError(t, err)
			require.True(t, preview.DataRequired)
			require.Empty(t, preview.Related)
		})
	}
}

type mysqlDMDateReader struct{ *mockReader }

func (r *mysqlDMDateReader) ReadPage(_ context.Context, table string, _ []string, offset, _ int64) ([]string, []string, [][]interface{}, error) {
	if offset > 0 {
		return []string{"equipment_validity"}, []string{"TIMESTAMP"}, nil, nil
	}
	return []string{"equipment_validity"}, []string{"TIMESTAMP"}, r.rows[table], nil
}

func TestMySQLDamengMigrationModes(t *testing.T) {
	for _, mode := range []string{"schema_only", "data_only", "both"} {
		t.Run(mode, func(t *testing.T) {
			zero := "0000-00-00 00:00:00"
			r := &mysqlDMDateReader{mockReader: &mockReader{
				tables: []string{"device_baseinfo"},
				ddl: map[string]*source.TableDDLInfo{"device_baseinfo": {
					TableName: "device_baseinfo", Columns: []source.ColumnInfo{
						{Name: "equipment_validity", DataType: "timestamp", Default: &zero},
					},
				}},
				rows: map[string][][]interface{}{"device_baseinfo": {{zero}}},
			}}
			w := &dmFixtureWriter{mockWriter: &mockWriter{}}
			m, job := newTestMigrator(r, w)
			m.cfg.Content = mode
			m.cfg.TargetSchema = "ZNFXPT"
			report := m.Run(context.Background())
			require.False(t, report.HasFailures())
			if mode == "data_only" {
				require.Empty(t, w.created)
			} else {
				require.Len(t, w.created, 1)
				require.Contains(t, w.created[0], `"EQUIPMENT_VALIDITY" TIMESTAMP NOT NULL DEFAULT '1900-01-01 00:00:00'`)
			}
			if mode == "schema_only" {
				require.Empty(t, w.copied)
			} else {
				require.Equal(t, []string{"device_baseinfo"}, w.copied)
			}
			require.Equal(t, "0000-00-00 00:00:00", zero, "source metadata must not be mutated")
			require.Contains(t, strings.Join(drainLogs(job), "\n"), "零日期数据统一替换为 1900-01-01 00:00:00")
		})
	}
}
