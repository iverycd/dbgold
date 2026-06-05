// datamigrate/migrator.go
package datamigrate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/typemap"
)

// Config 迁移任务配置
type Config struct {
	PageSize           int
	MaxParallel        int
	IntraTableParallel int // 单表内部分页并发数，<=1 表示串行
	Mode               string
	Filter             string
	Content            string // "both" | "schema_only" | "data_only"，默认 "both"
	LowerCaseNames     bool
	CharInLength       bool
	UseNvarchar2       bool
	Distributed        bool   // 分布式数据库：建主键前先执行 DISTRIBUTE BY hash
	TargetSchema       string // 目标库 schema，为空时使用连接默认 search_path
	ChangeOwner        bool   // 迁移后将表/视图/序列的 owner 改为 TargetSchema
	TargetDBType       string // "postgres" | "gaussdb" | "seabox"
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

var reGenRandomUUID = regexp.MustCompile(`(?i)\bgen_random_uuid\s*\(\s*\)`)

// adjustViewUUID 将视图定义中的中间形式 gen_random_uuid() 替换为目标库对应函数。
func adjustViewUUID(def, targetDBType string) string {
	switch targetDBType {
	case "gaussdb":
		return reGenRandomUUID.ReplaceAllString(def, "uuid()")
	case "seabox":
		return reGenRandomUUID.ReplaceAllString(def, "sys_guid()")
	default:
		return def
	}
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
	m.log.Infof("开始迁移任务，共 %d 张表，pageSize=%d，maxParallel=%d，intraTableParallel=%d",
		len(tables), m.cfg.PageSize, m.cfg.MaxParallel, m.cfg.IntraTableParallel)

	report.Tables.Total = len(tables)

	skipSchema := m.cfg.Content == "data_only"
	skipData := m.cfg.Content == "schema_only"

	// Phase 1: 建表 DDL（串行）
	tablesFailed := map[string]bool{}
	if skipSchema {
		m.log.Info("=== Phase 1: 跳过创建表结构（仅迁移数据行模式）===")
	} else {
		m.log.Info("=== Phase 1: 创建表结构 ===")
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
			if m.cfg.ChangeOwner && m.cfg.TargetSchema != "" {
				if err := m.writer.ChangeOwner(ctx, "TABLE", m.objName(table), m.cfg.TargetSchema); err != nil {
					m.log.Warnf("修改表 owner 失败 [%s]: %v", table, err)
				}
			}
		}
	}

	// Phase 2: 迁移数据（并发）
	report.Data.Total = len(tables) - len(tablesFailed)
	if skipData {
		m.log.Info("=== Phase 2: 跳过迁移数据（仅创建表结构模式）===")
	} else {
		m.log.Info("=== Phase 2: 迁移数据 ===")
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
	}

	// Phase 3: Post-DDL（串行）
	if skipSchema {
		m.log.Info("=== Phase 3: 跳过创建序列、索引、外键、视图（仅迁移数据行模式）===")
	} else {
		m.log.Info("=== Phase 3: 创建序列、索引、外键、视图 ===")
		m.createPostDDL(ctx, &report, tables, allTables)
	}

	// Phase 4: 行数对比（并发，仅对数据迁移成功的表）
	if skipData {
		m.log.Info("=== Phase 4: 跳过行数对比（未迁移数据）===")
	} else {
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
	}

	elapsed := time.Since(start).Round(time.Second)
	m.log.Donef("迁移完成：成功 %d 张，失败 %d 张，耗时 %s",
		report.Data.Success, report.Tables.Failed+report.Data.Failed, elapsed)
	return report
}

// mapColumnType 根据源库类型分发类型映射
func (m *Migrator) mapColumnType(col source.ColumnInfo) string {
	switch m.reader.DBType() {
	case "sqlserver":
		return typemap.SQLServerToPG(col, m.cfg.CharInLength, m.cfg.UseNvarchar2)
	case "dameng":
		return typemap.DaMengToPG(col, m.cfg.CharInLength, m.cfg.UseNvarchar2)
	case "oracle":
		return typemap.OracleToPG(col, m.cfg.CharInLength, m.cfg.UseNvarchar2)
	default: // mysql
		return typemap.MySQLToPG(col, m.cfg.CharInLength, m.cfg.UseNvarchar2)
	}
}

// buildCreateTableDDL 根据源库列信息生成目标库建表 DDL
func (m *Migrator) buildCreateTableDDL(ctx context.Context, table string) (string, error) {
	info, err := m.reader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return "", err
	}
	var cols []string
	for _, col := range info.Columns {
		pgType := m.mapColumnType(col)
		colDef := fmt.Sprintf(`"%s" %s`, m.objName(col.Name), pgType)
		if !col.IsNullable {
			colDef += " NOT NULL"
		}
		if col.Default != nil && col.Extra != "auto_increment" {
			def := *col.Default
			// SQL Server 默认值带额外括号，如 ((0))、(getdate())、(N'')，先剥离
			if m.reader.DBType() == "sqlserver" {
				def = stripSQLServerDefault(def)
			}
			// Oracle 默认值可能带多余单引号，如 ''0'' → 0，或 '''abc''' → 'abc'
			if m.reader.DBType() == "oracle" {
				def = stripOracleDefault(def)
			}
			// MySQL 8.0 表达式默认值在 information_schema 中以括号包裹，如 (' ') 或 (0)
			if m.reader.DBType() == "mysql" {
				def = stripMySQLExprDefault(def)
			}
			if isFunctionDefault(def) {
				colDef += fmt.Sprintf(" DEFAULT %s", pgFunctionDefault(def))
			} else {
				colDef += fmt.Sprintf(" DEFAULT '%s'", strings.ReplaceAll(def, "'", "''"))
			}
		}
		cols = append(cols, "  "+colDef)
	}
	tblName := m.objName(table)
	var qualifiedName string
	if m.cfg.TargetSchema != "" {
		qualifiedName = fmt.Sprintf(`"%s"."%s"`, m.cfg.TargetSchema, tblName)
	} else {
		qualifiedName = fmt.Sprintf(`"%s"`, tblName)
	}
	ddl := fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE;\nCREATE TABLE %s (\n%s\n);",
		qualifiedName, qualifiedName, strings.Join(cols, ",\n"))
	return ddl, nil
}

// migrateTableData 迁移单张表的数据，返回（是否成功，首次错误信息）
func (m *Migrator) migrateTableData(ctx context.Context, table string) (bool, string) {
	pks, err := m.reader.GetPrimaryKey(ctx, table)
	if err != nil {
		m.log.Errorf("获取主键失败 [%s]: %v", table, err)
		return false, err.Error()
	}
	if m.cfg.IntraTableParallel <= 1 {
		return m.migrateTableDataSerial(ctx, table, pks)
	}
	return m.migrateTableDataParallel(ctx, table, pks)
}

// migrateTableDataSerial 串行分页迁移单张表
func (m *Migrator) migrateTableDataSerial(ctx context.Context, table string, pks []string) (bool, string) {
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

// migrateTableDataParallel 并发分页迁移单张表
func (m *Migrator) migrateTableDataParallel(ctx context.Context, table string, pks []string) (bool, string) {
	totalRows, err := m.reader.CountRows(ctx, table)
	if err != nil {
		m.log.Errorf("获取行数失败 [%s]: %v", table, err)
		return false, err.Error()
	}
	if totalRows == 0 {
		m.log.Dataf("迁移 %s: 空表，跳过", table)
		return true, ""
	}

	pageSize := int64(m.cfg.PageSize)
	totalPages := (totalRows + pageSize - 1) / pageSize
	dstTable := m.objName(table)

	// 先读第一页以获取列名
	cols, firstRows, err := m.reader.ReadPage(ctx, table, pks, 0, pageSize)
	if err != nil {
		m.log.Errorf("读取数据失败 [%s] 第 1 页: %v", table, err)
		return false, err.Error()
	}
	dstCols := make([]string, len(cols))
	for i, c := range cols {
		dstCols[i] = m.objName(c)
	}

	var mu sync.Mutex
	firstErr := ""
	hasError := false

	writePage := func(pageNum int, rows [][]interface{}) {
		if err := m.writer.CopyData(ctx, dstTable, dstCols, rows); err != nil {
			m.log.Errorf("写入数据失败 [%s] 第 %d 页: %v", table, pageNum, err)
			mu.Lock()
			if !hasError {
				firstErr = err.Error()
				hasError = true
			}
			mu.Unlock()
		} else {
			m.log.Dataf("迁移 %s: 第 %d 页 (%d 行) ... OK", table, pageNum, len(rows))
		}
	}

	sem := make(chan struct{}, m.cfg.IntraTableParallel)
	var wg sync.WaitGroup

	// 第 1 页已读取，直接并发写入
	wg.Add(1)
	sem <- struct{}{}
	go func() {
		defer wg.Done()
		defer func() { <-sem }()
		writePage(1, firstRows)
	}()

	// 第 2 页起并发读取 + 写入
	for page := int64(1); page < totalPages; page++ {
		if ctx.Err() != nil {
			break
		}
		offset := page * pageSize
		pageNum := int(page) + 1
		wg.Add(1)
		sem <- struct{}{}
		go func(off int64, pn int) {
			defer wg.Done()
			defer func() { <-sem }()
			_, rows, err := m.reader.ReadPage(ctx, table, pks, off, pageSize)
			if err != nil {
				m.log.Errorf("读取数据失败 [%s] 第 %d 页: %v", table, pn, err)
				mu.Lock()
				if !hasError {
					firstErr = err.Error()
					hasError = true
				}
				mu.Unlock()
				return
			}
			if len(rows) == 0 {
				return
			}
			writePage(pn, rows)
		}(offset, pageNum)
	}

	wg.Wait()

	if ctx.Err() != nil && firstErr == "" {
		return false, "任务已取消"
	}
	if hasError {
		return false, firstErr
	}
	return true, ""
}

// createPostDDL 串行创建主键、序列、索引、外键、视图，并填充 report。
// tables 为本次迁移的表列表，allTables 为源库全部表列表（用于 exclude 模式推算被排除的表）。
func (m *Migrator) createPostDDL(ctx context.Context, report *MigrationReport, tables []string, allTables []string) {
	// tableSet: 本次要迁移的表集合
	tableSet := make(map[string]bool, len(tables))
	for _, t := range tables {
		tableSet[t] = true
	}
	// excludedSet: 被排除的表（仅 mode=exclude 时非空，用于过滤外键的 RefTable）
	var excludedSet map[string]bool
	if m.cfg.Mode == "exclude" {
		excludedSet = make(map[string]bool)
		for _, t := range allTables {
			if !tableSet[t] {
				excludedSet[t] = true
			}
		}
	}

	pks, err := m.reader.GetPrimaryKeys(ctx)
	if err != nil {
		m.log.Errorf("获取主键信息失败: %v", err)
	} else {
		filtered := make([]source.IndexInfo, 0, len(pks))
		for _, pk := range pks {
			if tableSet[pk.TableName] {
				filtered = append(filtered, pk)
			}
		}
		report.PrimaryKeys.Total = len(filtered)
		for _, pk := range filtered {
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
			if m.cfg.Distributed {
				if err := m.writer.AlterDistribute(ctx, pkCopy.TableName, pkCopy.Columns); err != nil {
					m.log.Errorf("设置分布列失败 [%s]: %v", pkCopy.TableName, err)
				}
			}
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
		filtered := make([]source.SequenceInfo, 0, len(seqs))
		for _, seq := range seqs {
			if tableSet[seq.TableName] {
				filtered = append(filtered, seq)
			}
		}
		report.Sequences.Total = len(filtered)
		for _, seq := range filtered {
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
				if m.cfg.ChangeOwner && m.cfg.TargetSchema != "" {
					seqObj := fmt.Sprintf("seq_%s_%s", seqCopy.TableName, seqCopy.ColumnName)
					if err := m.writer.ChangeOwner(ctx, "SEQUENCE", seqObj, m.cfg.TargetSchema); err != nil {
						m.log.Warnf("修改序列 owner 失败 [%s]: %v", seqObj, err)
					}
				}
			}
		}
	}

	indexes, err := m.reader.GetIndexes(ctx)
	if err != nil {
		m.log.Errorf("获取索引信息失败: %v", err)
	} else {
		filtered := make([]source.IndexInfo, 0, len(indexes))
		for _, idx := range indexes {
			if tableSet[idx.TableName] {
				filtered = append(filtered, idx)
			}
		}
		report.Indexes.Total = len(filtered)
		for _, idx := range filtered {
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

	// include 模式：跳过外键（引用表可能不在迁移范围内）
	if m.cfg.Mode != "include" {
		fks, err := m.reader.GetForeignKeys(ctx)
		if err != nil {
			m.log.Errorf("获取外键信息失败: %v", err)
		} else {
			filtered := make([]source.FKInfo, 0, len(fks))
			for _, fk := range fks {
				if excludedSet[fk.TableName] || excludedSet[fk.RefTable] {
					continue
				}
				filtered = append(filtered, fk)
			}
			report.Constraints.Total = len(filtered)
			for _, fk := range filtered {
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
	}

	// include 模式：跳过视图（依赖表可能不在迁移范围内）
	if m.cfg.Mode != "include" {
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
				vCopy.Definition = adjustViewUUID(v.Definition, m.cfg.TargetDBType)
				// 拼出完整 DDL 用于报告展示，与 writer.CreateView 实际执行的语句一致
				viewDDL := fmt.Sprintf("CREATE OR REPLACE VIEW \"%s\" AS\n%s;", vCopy.ViewName, vCopy.Definition)
				if m.cfg.TargetSchema != "" {
					viewDDL = fmt.Sprintf("CREATE OR REPLACE VIEW \"%s\".\"%s\" AS\n%s;", m.cfg.TargetSchema, vCopy.ViewName, vCopy.Definition)
				}
				if err := m.writer.CreateView(ctx, vCopy); err != nil {
					m.log.Errorf("创建视图失败 [%s]: %v", vCopy.ViewName, err)
					report.Views.Failed++
					report.Views.Items = append(report.Views.Items, ObjectResult{
						Name:  vCopy.ViewName,
						DDL:   viewDDL,
						Error: err.Error(),
					})
				} else {
					m.log.DDLf("创建视图 %s ... OK", vCopy.ViewName)
					report.Views.Success++
					if m.cfg.ChangeOwner && m.cfg.TargetSchema != "" {
						if err := m.writer.ChangeOwner(ctx, "VIEW", vCopy.ViewName, m.cfg.TargetSchema); err != nil {
							m.log.Warnf("修改视图 owner 失败 [%s]: %v", vCopy.ViewName, err)
						}
					}
				}
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

// pgFunctionDefault 将函数默认值映射到 PostgreSQL 等价形式（兼容 MySQL 和 SQL Server）
func pgFunctionDefault(def string) string {
	upper := strings.ToUpper(strings.TrimSpace(def))
	switch upper {
	case "CURRENT_TIMESTAMP", "NOW()", "GETDATE()":
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
	case "NEWID()":
		return "gen_random_uuid()"
	case "UUID()":
		return "gen_random_uuid()"
	default:
		return def
	}
}

// stripOracleDefault 清理 Oracle 默认值中多余的单引号。
// Oracle 在 ALL_TAB_COLUMNS.DATA_DEFAULT 里原样保存 SQL 字面量，如：
//
//	''0''   → 0      (bigint 列的数字默认值)
//	'''abc''' → 'abc' (字符串默认值，外层再包一层引号)
//	NULL    → 保持不变
func stripOracleDefault(def string) string {
	def = strings.TrimSpace(def)
	// 连续两个单引号包裹：''value'' → value（用于数字或裸值）
	if len(def) >= 4 && strings.HasPrefix(def, "''") && strings.HasSuffix(def, "''") {
		inner := def[2 : len(def)-2]
		// 确保内部没有单引号（否则是字符串默认值，不做处理）
		if !strings.Contains(inner, "'") {
			return strings.TrimSpace(inner)
		}
	}
	// 三层单引号：'''value''' → 'value'（字符串默认值）
	if len(def) >= 6 && strings.HasPrefix(def, "'''") && strings.HasSuffix(def, "'''") {
		return def[2 : len(def)-2]
	}
	// 单层单引号：'value' → value（Oracle 直接存储 SQL 字面量，外层格式化会补引号）
	if len(def) >= 2 && strings.HasPrefix(def, "'") && strings.HasSuffix(def, "'") {
		inner := def[1 : len(def)-1]
		return strings.ReplaceAll(inner, "''", "'")
	}
	return def
}

// SQL Server 默认值通常带额外括号，如 ((0)) → 0，(getdate()) → getdate()，(N'abc') → abc
// SQL Server 默认值通常带额外括号，如 ((0)) → 0，(getdate()) → getdate()，(N'abc') → abc。
func stripSQLServerDefault(def string) string {
	def = strings.TrimSpace(def)
	// 循环剥离最外层括号（只要整体被括号包围）
	for strings.HasPrefix(def, "(") && strings.HasSuffix(def, ")") {
		inner := def[1 : len(def)-1]
		if isBalancedParens(inner) {
			def = strings.TrimSpace(inner)
		} else {
			break
		}
	}
	// 去掉 N'...' 前缀（SQL Server Unicode 字符串字面量）
	if strings.HasPrefix(def, "N'") && strings.HasSuffix(def, "'") {
		def = def[1:] // 去掉 N，保留 '...'
	}
	return def
}

// isBalancedParens 检查字符串中括号是否平衡
func isBalancedParens(s string) bool {
	depth := 0
	for _, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// stripMySQLExprDefault 剥离 MySQL 8.0 在 information_schema 中给表达式默认值加的外层括号。
// MySQL 8.0 将 DEFAULT ' ' 存储为 (' ')，DEFAULT 0 存储为 (0)。
// 剥括号后若内部是 SQL 字符串字面量（如 ' '），再去掉引号返回裸值，
// 让上层的 DEFAULT '%s' 路径正确拼出 DEFAULT ' '。
func stripMySQLExprDefault(def string) string {
	def = strings.TrimSpace(def)
	if strings.HasPrefix(def, "(") && strings.HasSuffix(def, ")") {
		inner := strings.TrimSpace(def[1 : len(def)-1])
		if isBalancedParens(inner) {
			def = inner
		}
	}
	// 内部是 SQL 字符串字面量 'value'，提取裸值交由上层统一处理
	if len(def) >= 2 && strings.HasPrefix(def, "'") && strings.HasSuffix(def, "'") {
		inner := def[1 : len(def)-1]
		return strings.ReplaceAll(inner, "''", "'")
	}
	return def
}
