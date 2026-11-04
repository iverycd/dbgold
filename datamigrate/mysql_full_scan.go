package datamigrate

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"dbgold/datamigrate/source"
)

type mysqlTableStreamer interface {
	// The consumer may retain the batch after returning, including its byte values.
	StreamMySQLTable(context.Context, string, *source.MySQLScanRange, int, func([]string, []string, [][]interface{}) error) error
}

type mysqlRangePlanner interface {
	MySQLScanRanges(context.Context, string, int) ([]source.MySQLScanRange, error)
}

func (m *Migrator) migrateMySQLScan(taskCtx context.Context, table string) (bool, string) {
	reader, ok := m.reader.(mysqlTableStreamer)
	if !ok {
		return false, fmt.Sprintf("表 %s 的源库不支持 MySQL 流式读取", table)
	}
	limit := max(1, m.cfg.IntraTableParallel)
	var spans []source.MySQLScanRange
	switch m.cfg.MySQLReadStrategy {
	case "stream":
	case "range":
		if planner, ok := m.reader.(mysqlRangePlanner); ok && limit > 1 {
			var err error
			spans, err = planner.MySQLScanRanges(taskCtx, table, limit)
			if err != nil {
				m.stopMigrationTask(taskCtx, err)
				message := fmt.Sprintf("读取主键范围失败 [%s]: %v", table, err)
				m.log.Errorf("%s", message)
				return false, message
			}
		}
	default:
		return false, fmt.Sprintf("未知 MySQL 读取策略 %q", m.cfg.MySQLReadStrategy)
	}

	ctx, cancel := context.WithCancelCause(taskCtx)
	defer cancel(nil)
	// A table error stops this table's readers and writers. Only connection and
	// other task-level errors may cancel the job and its independent tables.
	fail := func(err error, diagnostic string) {
		if ctx.Err() == nil {
			m.stopMigrationTask(taskCtx, err)
			cancel(fmt.Errorf("%s", diagnostic))
		}
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, limit)
	dispatch := func(work func()) error {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		wg.Go(func() {
			defer func() { <-slots }()
			if ctx.Err() == nil {
				work()
			}
		})
		return nil
	}

	dstTable := m.objName(table)
	var batches atomic.Int64
	consume := func(columns, types []string, rows [][]interface{}) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		batch := batches.Add(1)
		mapped := make([]string, len(columns))
		for i, column := range columns {
			mapped[i] = m.targetColumn(table, column)
		}
		write := func() {
			if err := m.writer.CopyData(ctx, dstTable, mapped, types, rows); err != nil {
				fail(err, fmt.Sprintf("表 %s 第 %d 批写入失败: %s", table, batch, formatDataWriteError(err)))
			} else {
				m.log.Dataf("迁移 %s: 第 %d 批 (%d 行) ... OK", table, batch, len(rows))
			}
		}
		if len(spans) == 0 {
			// One cursor feeds at most limit active writes. When all slots are
			// occupied the cursor blocks with just one more batch in memory.
			return dispatch(write)
		}
		// Each range owns one slot for its whole read/write loop. There is
		// no nested writer pool multiplying the table's concurrency limit.
		write()
		return context.Cause(ctx)
	}
	scan := func(span *source.MySQLScanRange) {
		if err := reader.StreamMySQLTable(ctx, table, span, m.cfg.PageSize, consume); err != nil {
			fail(err, fmt.Sprintf("读取数据失败 [%s]: %v", table, err))
		}
	}
	if len(spans) == 0 {
		scan(nil)
	} else {
		for i := range spans {
			if err := dispatch(func() { scan(&spans[i]) }); err != nil {
				break
			}
		}
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		m.log.Errorf("%s", err)
		return false, err.Error()
	}
	if batches.Load() == 0 {
		m.log.Dataf("迁移 %s: 空表 ... OK", table)
	}
	return true, ""
}
