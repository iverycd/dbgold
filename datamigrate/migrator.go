// datamigrate/migrator.go
package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

// Config 迁移任务配置
type Config struct {
	PageSize    int
	MaxParallel int
	Mode        string
	Filter      string
}

// Migrator 串联三阶段迁移：DDL → 数据 → Post-DDL
type Migrator struct {
	reader source.Reader
	writer target.Writer
	job    *Job
	cfg    Config
	log    *Logger
}

// NewMigrator 创建 Migrator
func NewMigrator(reader source.Reader, writer target.Writer, job *Job, cfg Config) *Migrator {
	return &Migrator{reader: reader, writer: writer, job: job, cfg: cfg, log: NewLogger(job.LogCh)}
}

// Run 执行完整的三阶段迁移，返回 MigrationReport；结束时不关闭 job.LogCh（由调用方关闭）
func (m *Migrator) Run(ctx context.Context) MigrationReport {
	report := newMigrationReport()
	start := time.Now()

	// 查询触发器总数（失败时 Total=-1，前端展示"获取失败"）
	if count, err := m.reader.GetTriggerCount(ctx); err != nil {
		report.Triggers.Total = -1
	} else {
		report.Triggers.Total = count
	}

	if err := ctx.Err(); err != nil {
		m.log.Warn("任务已取消")
		return report
	}

	allTables, err := m.reader.ListTables(ctx)
	if err != nil {
		m.log.Errorf("获取表列表失败: %v", err)
		return report
	}
	tables := FilterTables(allTables, m.cfg.Mode, m.cfg.Filter)
	m.log.Infof("开始迁移任务，共 %d 张表，pageSize=%d，maxParallel=%d",
		len(tables), m.cfg.PageSize, m.cfg.MaxParallel)

	report.Tables.Total = len(tables)

	// Phase 1: 建表 DDL（串行）
	m.log.Info("=== Phase 1: 创建表结构 ===")
	tablesFailed := map[string]bool{}
	for _, table := range tables {
		if ctx.Err() != nil {
			m.log.Warn("任务已取消")
			return report
		}
		ddl, err := m.buildCreateTableDDL(ctx, table)
		if err != nil {
			m.log.Errorf("生成建表 DDL 失败 [%s]: %v", table, err)
			tablesFailed[table] = true
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, DDL: "", Error: err.Error()})
			continue
		}
		if err := m.writer.CreateTable(ctx, ddl); err != nil {
			m.log.Errorf("创建表失败 [%s]: %v", table, err)
			tablesFailed[table] = true
			report.Tables.Failed++
			report.Tables.Items = append(report.Tables.Items, ObjectResult{Name: table, DDL: ddl, Error: err.Error()})
			continue
		}
		m.log.DDLf("创建表 %s ... OK", table)
		report.Tables.Success++
	}

	// Phase 2: 迁移数据（并发）
	m.log.Info("=== Phase 2: 迁移数据 ===")
	report.Data.Total = len(tables) - len(tablesFailed)
	sem := make(chan struct{}, m.cfg.MaxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, table := range tables {
		if tablesFailed[table] {
			continue
		}
		if ctx.Err() != nil {
			m.log.Warn("任务已取消")
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(tbl string) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, firstErr := m.migrateTableData(ctx, tbl)
			mu.Lock()
			if ok {
				report.Data.Success++
			} else {
				report.Data.Failed++
				report.Data.Items = append(report.Data.Items, ObjectResult{Name: tbl, DDL: "", Error: firstErr})
			}
			mu.Unlock()
		}(table)
	}
	wg.Wait()

	// Phase 3: Post-DDL（串行）
	m.log.Info("=== Phase 3: 创建序列、索引、外键、视图 ===")
	m.createPostDDL(ctx, &report)

	elapsed := time.Since(start).Round(time.Second)
	m.log.Donef("迁移完成：成功 %d 张，失败 %d 张，耗时 %s",
		report.Data.Success, report.Tables.Failed+report.Data.Failed, elapsed)
	return report
}

// buildCreateTableDDL 根据源库列信息生成目标库建表 DDL
func (m *Migrator) buildCreateTableDDL(ctx context.Context, table string) (string, error) {
	info, err := m.reader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return "", err
	}
	var cols []string
	for _, col := range info.Columns {
		pgType := typemap.MySQLToPG(col)
		colDef := fmt.Sprintf(`"%s" %s`, col.Name, pgType)
		if !col.IsNullable {
			colDef += " NOT NULL"
		}
		if col.Default != nil && col.Extra != "auto_increment" {
			escapedDefault := strings.ReplaceAll(*col.Default, "'", "''")
			colDef += fmt.Sprintf(" DEFAULT '%s'", escapedDefault)
		}
		cols = append(cols, "  "+colDef)
	}
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS \"%s\";\nCREATE TABLE \"%s\" (\n%s\n);",
		table, table, strings.Join(cols, ",\n"))
	return ddl, nil
}

// migrateTableData 迁移单张表的数据，返回（是否成功，首次错误信息）
func (m *Migrator) migrateTableData(ctx context.Context, table string) (bool, string) {
	pk, err := m.reader.GetPrimaryKey(ctx, table)
	if err != nil {
		m.log.Errorf("获取主键失败 [%s]: %v", table, err)
		return false, err.Error()
	}
	var offset int64
	pageNum := 0
	firstErr := ""
	hasError := false
	for {
		if ctx.Err() != nil {
			if firstErr == "" {
				firstErr = "任务已取消"
			}
			return false, firstErr
		}
		cols, rows, err := m.reader.ReadPage(ctx, table, pk, offset, int64(m.cfg.PageSize))
		if err != nil {
			m.log.Errorf("读取数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			return false, err.Error()
		}
		if len(rows) == 0 {
			break
		}
		if err := m.writer.CopyData(ctx, table, cols, rows); err != nil {
			m.log.Errorf("写入数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			if !hasError {
				firstErr = err.Error()
				hasError = true
			}
			offset += int64(m.cfg.PageSize)
			continue
		}
		pageNum++
		m.log.Dataf("迁移 %s: 第 %d 页 (%d 行) ... OK", table, pageNum, len(rows))
		if len(rows) < m.cfg.PageSize {
			break
		}
		offset += int64(m.cfg.PageSize)
	}
	if hasError {
		return false, firstErr
	}
	return true, ""
}

// createPostDDL 串行创建序列、索引、外键、视图，并填充 report
func (m *Migrator) createPostDDL(ctx context.Context, report *MigrationReport) {
	seqs, err := m.reader.GetSequences(ctx)
	if err != nil {
		m.log.Errorf("获取序列信息失败: %v", err)
	} else {
		report.Sequences.Total = len(seqs)
		for _, seq := range seqs {
			if ctx.Err() != nil {
				return
			}
			ddl := SequenceDDL(seq)
			if err := m.writer.CreateSequence(ctx, seq); err != nil {
				m.log.Errorf("创建序列失败 [%s.%s]: %v", seq.TableName, seq.ColumnName, err)
				report.Sequences.Failed++
				report.Sequences.Items = append(report.Sequences.Items, ObjectResult{
					Name:  fmt.Sprintf("%s.%s", seq.TableName, seq.ColumnName),
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建序列 seq_%s_%s ... OK", seq.TableName, seq.ColumnName)
				report.Sequences.Success++
			}
		}
	}

	indexes, err := m.reader.GetIndexes(ctx)
	if err != nil {
		m.log.Errorf("获取索引信息失败: %v", err)
	} else {
		report.Indexes.Total = len(indexes)
		for _, idx := range indexes {
			if ctx.Err() != nil {
				return
			}
			ddl := IndexDDL(idx)
			if err := m.writer.CreateIndex(ctx, idx); err != nil {
				m.log.Errorf("创建索引失败 [%s]: %v", idx.IndexName, err)
				report.Indexes.Failed++
				report.Indexes.Items = append(report.Indexes.Items, ObjectResult{
					Name:  idx.IndexName,
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建索引 %s ... OK", idx.IndexName)
				report.Indexes.Success++
			}
		}
	}

	fks, err := m.reader.GetForeignKeys(ctx)
	if err != nil {
		m.log.Errorf("获取外键信息失败: %v", err)
	} else {
		report.Constraints.Total = len(fks)
		for _, fk := range fks {
			if ctx.Err() != nil {
				return
			}
			ddl := FKDDL(fk)
			if err := m.writer.CreateForeignKey(ctx, fk); err != nil {
				m.log.Errorf("创建外键失败 [%s]: %v", fk.ConstraintName, err)
				report.Constraints.Failed++
				report.Constraints.Items = append(report.Constraints.Items, ObjectResult{
					Name:  fk.ConstraintName,
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建外键 %s ... OK", fk.ConstraintName)
				report.Constraints.Success++
			}
		}
	}

	views, err := m.reader.GetViews(ctx)
	if err != nil {
		m.log.Errorf("获取视图信息失败: %v", err)
	} else {
		report.Views.Total = len(views)
		for _, v := range views {
			if ctx.Err() != nil {
				return
			}
			if err := m.writer.CreateView(ctx, v); err != nil {
				m.log.Errorf("创建视图失败 [%s]: %v", v.ViewName, err)
				report.Views.Failed++
				report.Views.Items = append(report.Views.Items, ObjectResult{
					Name:  v.ViewName,
					DDL:   v.Definition,
					Error: err.Error(),
				})
			} else {
				m.log.DDLf("创建视图 %s ... OK", v.ViewName)
				report.Views.Success++
			}
		}
	}
}
