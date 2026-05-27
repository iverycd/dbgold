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
	PageSize       int
	MaxParallel    int
	Mode           string
	Filter         string
	LowerCaseNames bool
	CharInLength   bool
	UseNvarchar2   bool
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

// objName 根据 LowerCaseNames 配置决定是否将对象名转为小写
func (m *Migrator) objName(s string) string {
	if m.cfg.LowerCaseNames {
		return strings.ToLower(s)
	}
	return s
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

	// Phase 4: 行数对比（并发，仅对数据迁移成功的表）
	m.log.Info("=== Phase 4: 行数对比 ===")
	var successTables []string
	for _, table := range tables {
		if !tablesFailed[table] {
			successTables = append(successTables, table)
		}
	}
	rowCounts := make([]TableRowCount, len(successTables))
	var rcWg sync.WaitGroup
	rcSem := make(chan struct{}, m.cfg.MaxParallel)
	for i, table := range successTables {
		rcWg.Add(1)
		rcSem <- struct{}{}
		go func(idx int, tbl string) {
			defer rcWg.Done()
			defer func() { <-rcSem }()
			dstTable := m.objName(tbl)
			srcCount, srcErr := m.reader.CountRows(ctx, tbl)
			dstCount, dstErr := m.writer.CountRows(ctx, dstTable)
			rc := TableRowCount{Table: dstTable}
			if srcErr == nil && dstErr == nil {
				rc.Src = srcCount
				rc.Dst = dstCount
				rc.Match = srcCount == dstCount
				if !rc.Match {
					m.log.Warnf("行数不一致 [%s]: 源=%d 目标=%d", tbl, srcCount, dstCount)
				}
			}
			rowCounts[idx] = rc
		}(i, table)
	}
	rcWg.Wait()
	report.RowCounts = rowCounts

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
		pgType := typemap.MySQLToPG(col, m.cfg.CharInLength, m.cfg.UseNvarchar2)
		colDef := fmt.Sprintf(`"%s" %s`, m.objName(col.Name), pgType)
		if !col.IsNullable {
			colDef += " NOT NULL"
		}
		if col.Default != nil && col.Extra != "auto_increment" {
			def := *col.Default
			if isFunctionDefault(def) {
				colDef += fmt.Sprintf(" DEFAULT %s", pgFunctionDefault(def))
			} else {
				colDef += fmt.Sprintf(" DEFAULT '%s'", strings.ReplaceAll(def, "'", "''"))
			}
		}
		cols = append(cols, "  "+colDef)
	}
	tblName := m.objName(table)
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS \"%s\" CASCADE;\nCREATE TABLE \"%s\" (\n%s\n);",
		tblName, tblName, strings.Join(cols, ",\n"))
	return ddl, nil
}

// migrateTableData 迁移单张表的数据，返回（是否成功，首次错误信息）
func (m *Migrator) migrateTableData(ctx context.Context, table string) (bool, string) {
	pks, err := m.reader.GetPrimaryKey(ctx, table)
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
		cols, rows, err := m.reader.ReadPage(ctx, table, pks, offset, int64(m.cfg.PageSize))
		if err != nil {
			m.log.Errorf("读取数据失败 [%s] 第 %d 页: %v", table, pageNum+1, err)
			return false, err.Error()
		}
		if len(rows) == 0 {
			break
		}
		dstTable := m.objName(table)
		dstCols := make([]string, len(cols))
		for i, c := range cols {
			dstCols[i] = m.objName(c)
		}
		if err := m.writer.CopyData(ctx, dstTable, dstCols, rows); err != nil {
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

// createPostDDL 串行创建主键、序列、索引、外键、视图，并填充 report
func (m *Migrator) createPostDDL(ctx context.Context, report *MigrationReport) {
	pks, err := m.reader.GetPrimaryKeys(ctx)
	if err != nil {
		m.log.Errorf("获取主键信息失败: %v", err)
	} else {
		report.PrimaryKeys.Total = len(pks)
		for _, pk := range pks {
			if ctx.Err() != nil {
				return
			}
			pkCopy := pk
			pkCopy.TableName = m.objName(pk.TableName)
			cols := make([]string, len(pk.Columns))
			for i, c := range pk.Columns {
				cols[i] = m.objName(c)
			}
			pkCopy.Columns = cols
			ddl := IndexDDL(pkCopy)
			if err := m.writer.CreateIndex(ctx, pkCopy); err != nil {
				m.log.Errorf("创建主键失败 [%s]: %v", pkCopy.TableName, err)
				report.PrimaryKeys.Failed++
				report.PrimaryKeys.Items = append(report.PrimaryKeys.Items, ObjectResult{Name: pkCopy.TableName, DDL: ddl, Error: err.Error()})
			} else {
				m.log.Indexf("创建主键 %s ... OK", pkCopy.TableName)
				report.PrimaryKeys.Success++
			}
		}
	}

	seqs, err := m.reader.GetSequences(ctx)
	if err != nil {
		m.log.Errorf("获取序列信息失败: %v", err)
	} else {
		report.Sequences.Total = len(seqs)
		for _, seq := range seqs {
			if ctx.Err() != nil {
				return
			}
			seqCopy := seq
			seqCopy.TableName = m.objName(seq.TableName)
			seqCopy.ColumnName = m.objName(seq.ColumnName)
			ddl := SequenceDDL(seqCopy)
			if err := m.writer.CreateSequence(ctx, seqCopy); err != nil {
				m.log.Errorf("创建序列失败 [%s.%s]: %v", seqCopy.TableName, seqCopy.ColumnName, err)
				report.Sequences.Failed++
				report.Sequences.Items = append(report.Sequences.Items, ObjectResult{
					Name:  fmt.Sprintf("%s.%s", seqCopy.TableName, seqCopy.ColumnName),
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建序列 seq_%s_%s ... OK", seqCopy.TableName, seqCopy.ColumnName)
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
			idxCopy := idx
			idxCopy.TableName = m.objName(idx.TableName)
			idxCopy.IndexName = m.objName(idx.IndexName)
			cols := make([]string, len(idx.Columns))
			for i, c := range idx.Columns {
				cols[i] = m.objName(c)
			}
			idxCopy.Columns = cols
			ddl := IndexDDL(idxCopy)
			if err := m.writer.CreateIndex(ctx, idxCopy); err != nil {
				m.log.Errorf("创建索引失败 [%s]: %v", idxCopy.IndexName, err)
				report.Indexes.Failed++
				report.Indexes.Items = append(report.Indexes.Items, ObjectResult{
					Name:  idxCopy.IndexName,
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建索引 %s ... OK", idxCopy.IndexName)
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
			fkCopy := fk
			fkCopy.TableName = m.objName(fk.TableName)
			fkCopy.ConstraintName = m.objName(fk.ConstraintName)
			fkCols := make([]string, len(fk.Columns))
			for i, c := range fk.Columns {
				fkCols[i] = m.objName(c)
			}
			fkCopy.Columns = fkCols
			fkCopy.RefTable = m.objName(fk.RefTable)
			refCols := make([]string, len(fk.RefColumns))
			for i, c := range fk.RefColumns {
				refCols[i] = m.objName(c)
			}
			fkCopy.RefColumns = refCols
			ddl := FKDDL(fkCopy)
			if err := m.writer.CreateForeignKey(ctx, fkCopy); err != nil {
				m.log.Errorf("创建外键失败 [%s]: %v", fkCopy.ConstraintName, err)
				report.Constraints.Failed++
				report.Constraints.Items = append(report.Constraints.Items, ObjectResult{
					Name:  fkCopy.ConstraintName,
					DDL:   ddl,
					Error: err.Error(),
				})
			} else {
				m.log.Indexf("创建外键 %s ... OK", fkCopy.ConstraintName)
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
			vCopy := v
			vCopy.ViewName = m.objName(v.ViewName)
			if err := m.writer.CreateView(ctx, vCopy); err != nil {
				m.log.Errorf("创建视图失败 [%s]: %v", vCopy.ViewName, err)
				report.Views.Failed++
				report.Views.Items = append(report.Views.Items, ObjectResult{
					Name:  vCopy.ViewName,
					DDL:   vCopy.Definition,
					Error: err.Error(),
				})
			} else {
				m.log.DDLf("创建视图 %s ... OK", vCopy.ViewName)
				report.Views.Success++
			}
		}
	}
}

// isFunctionDefault 判断默认值是否为函数或关键字（不应加引号）
func isFunctionDefault(def string) bool {
	upper := strings.ToUpper(strings.TrimSpace(def))
	keywords := []string{
		"CURRENT_TIMESTAMP", "NOW()", "CURRENT_DATE", "CURRENT_TIME",
		"NULL", "TRUE", "FALSE",
	}
	for _, kw := range keywords {
		if upper == kw {
			return true
		}
	}
	// 以括号结尾的视为函数调用
	return strings.HasSuffix(upper, ")")
}

// pgFunctionDefault 将 MySQL 函数默认值映射到 PostgreSQL 等价形式
func pgFunctionDefault(def string) string {
	upper := strings.ToUpper(strings.TrimSpace(def))
	switch upper {
	case "CURRENT_TIMESTAMP", "NOW()":
		return "CURRENT_TIMESTAMP"
	case "CURRENT_DATE":
		return "CURRENT_DATE"
	case "CURRENT_TIME":
		return "CURRENT_TIME"
	case "NULL":
		return "NULL"
	case "TRUE":
		return "TRUE"
	case "FALSE":
		return "FALSE"
	default:
		return def
	}
}
