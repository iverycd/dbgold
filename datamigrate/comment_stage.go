package datamigrate

import (
	"context"
	"time"
)

type commentPersistenceSnapshot struct {
	duration time.Duration
	saves    int
	failures int
}

func (m *Migrator) snapshotCommentPersistence() commentPersistenceSnapshot {
	if m.repairs == nil {
		return commentPersistenceSnapshot{}
	}
	r := m.repairs
	r.mu.Lock()
	defer r.mu.Unlock()
	return commentPersistenceSnapshot{
		duration: r.saveDuration,
		// The recorder's successful-save counters exclude failed attempts.
		saves:    r.fullSaves + r.objectSaves + r.saveFailures,
		failures: r.saveFailures,
	}
}

func (m *Migrator) logCommentRead(elapsed time.Duration, count int, err error) {
	status := "成功"
	if err != nil {
		status = "失败"
	}
	m.log.Infof("注释读取：%d 条，耗时 %s，状态：%s", count, elapsed.Round(time.Millisecond), status)
}

// The returned callback also reports partial progress when cancellation exits
// the otherwise serial comment loop. Counters exclude earlier repair stages.
func (m *Migrator) startCommentCreationStats(ctx context.Context, category *CategoryReport, now func() time.Time) func() {
	started := now()
	before := m.snapshotCommentPersistence()
	initialSuccess, initialFailed := category.Success, category.Failed
	return func() {
		elapsed := now().Sub(started)
		after := m.snapshotCommentPersistence()
		success, failed := category.Success-initialSuccess, category.Failed-initialFailed
		processed := success + failed
		var perSecond, millisecondsPerComment float64
		if elapsed > 0 && processed > 0 {
			perSecond = float64(processed) / elapsed.Seconds()
			millisecondsPerComment = float64(elapsed) / float64(time.Millisecond) / float64(processed)
		}
		status := "完成"
		if ctx.Err() != nil {
			status = "已取消"
		}
		m.log.Infof("注释创建阶段%s：耗时 %s，处理 %d 条，成功 %d，失败 %d，平均 %.2f 条/秒（%.2f ms/条）；修复记录保存耗时 %s，保存 %d 次，失败 %d 次",
			status, elapsed.Round(time.Millisecond), processed, success, failed, perSecond, millisecondsPerComment,
			(after.duration - before.duration).Round(time.Millisecond), after.saves-before.saves, after.failures-before.failures)
	}
}
