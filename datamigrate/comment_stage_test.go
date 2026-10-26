package datamigrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestCommentCreationStatsOnlyCountsCurrentStage(t *testing.T) {
	m, job := newTestMigrator(&mockReader{}, &mockWriter{})
	m.repairs = &repairRecorder{saveDuration: time.Second, fullSaves: 3, objectSaves: 10, saveFailures: 2}
	category := CategoryReport{Success: 8, Failed: 4}
	now := time.Unix(100, 0)
	finish := m.startCommentCreationStats(context.Background(), &category, func() time.Time { return now })
	now = now.Add(2 * time.Second)
	category.Success += 2
	category.Failed++
	m.repairs.saveDuration += 300 * time.Millisecond
	m.repairs.fullSaves++
	m.repairs.objectSaves += 2
	m.repairs.saveFailures++
	finish()
	logs := strings.Join(drainLogs(job), "\n")
	require.Contains(t, logs, "注释创建阶段完成：耗时 2s，处理 3 条，成功 2，失败 1，平均 1.50 条/秒（666.67 ms/条）")
	require.Contains(t, logs, "修复记录保存耗时 300ms，保存 4 次，失败 1 次")
}

type commentStageReader struct {
	*mockReader
	comments []source.CommentInfo
	err      error
}

func (r *commentStageReader) GetComments(context.Context) ([]source.CommentInfo, error) {
	return r.comments, r.err
}

type commentStageWriter struct {
	*mockWriter
	calls    []source.CommentInfo
	onCreate func(source.CommentInfo) error
}

func (w *commentStageWriter) CreateComment(_ context.Context, comment source.CommentInfo) error {
	w.calls = append(w.calls, comment)
	if w.onCreate != nil {
		return w.onCreate(comment)
	}
	return nil
}

func TestCommentStageStatsPreservesExecutionAndCancellation(t *testing.T) {
	comments := []source.CommentInfo{
		{TableName: "Orders", Comment: "orders"},
		{TableName: "Orders", ColumnName: "Title", Comment: "title"},
		{TableName: "Excluded", Comment: "outside this migration"},
	}
	for _, test := range []struct {
		name       string
		comments   []source.CommentInfo
		readError  error
		cancel     bool
		writeError bool
		calls      int
		success    int
		failed     int
	}{
		{name: "success", comments: comments, calls: 2, success: 2},
		{name: "write_failure_continues", comments: comments, writeError: true, calls: 2, success: 1, failed: 1},
		{name: "zero_comments"},
		{name: "read_failure", readError: errors.New("catalog unavailable"), failed: 1},
		{name: "cancel_during_creation", comments: comments, cancel: true, calls: 1, success: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &commentStageReader{mockReader: &mockReader{}, comments: test.comments, err: test.readError}
			writer := &commentStageWriter{mockWriter: &mockWriter{}}
			writer.onCreate = func(comment source.CommentInfo) error {
				if test.cancel {
					cancel()
				}
				if test.writeError && comment.ColumnName == "" {
					return errors.New("comment rejected")
				}
				return nil
			}
			m, job := newTestMigrator(reader, writer)
			m.cfg.Objects = []string{"comments"}
			report := newMigrationReport()
			m.createPostDDL(ctx, &report, []string{"Orders"}, []string{"Orders", "Excluded"})
			require.Len(t, writer.calls, test.calls)
			require.Equal(t, test.success, report.Comments.Success)
			require.Equal(t, test.failed, report.Comments.Failed)
			for i, call := range writer.calls {
				require.Equal(t, comments[i], call, "execution order and source spelling must remain unchanged")
			}
			logs := strings.Join(drainLogs(job), "\n")
			require.Contains(t, logs, "注释读取：")
			if test.readError != nil {
				require.True(t, report.Comments.TotalUnknown)
				require.Contains(t, logs, "状态：失败")
				require.Contains(t, logs, "获取注释信息失败: catalog unavailable")
				require.NotContains(t, logs, "注释创建阶段")
				return
			}
			require.Equal(t, test.calls, report.Comments.Total)
			require.Contains(t, logs, "状态：成功")
			require.Contains(t, logs, "修复记录保存耗时 0s，保存 0 次，失败 0 次")
			if test.cancel {
				require.Contains(t, logs, "注释创建阶段已取消：")
			} else {
				require.Contains(t, logs, "注释创建阶段完成：")
			}
			if len(test.comments) == 0 {
				require.Contains(t, logs, "处理 0 条，成功 0，失败 0，平均 0.00 条/秒（0.00 ms/条）")
			}
		})
	}
}
