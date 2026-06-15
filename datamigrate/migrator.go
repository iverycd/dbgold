// datamigrate/migrator.go
package datamigrate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
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

// sourceTypeSetter 由 Writer 可选实现,用于接收源库类型以驱动 ValueConverter。
type sourceTypeSetter interface {
	SetSourceType(srcType string)
}

// NewMigrator 创建 Migrator
func NewMigrator(reader source.Reader, writer target.Writer, job *Job, cfg Config) *Migrator {
	// 若 Writer 支持,注入源库类型供其 ValueConverter 落地中立值
	if s, ok := writer.(sourceTypeSetter); ok {
		s.SetSourceType(reader.DBType())
	}
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

// buildCreateTableDDL 通过目标方言生成建表 DDL。
func (m *Migrator) buildCreateTableDDL(ctx context.Context, table string) (string, error) {
	info, err := m.reader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return "", err
	}
	opt := dialect.TypeOpt{CharInLength: m.cfg.CharInLength, UseNvarchar2: m.cfg.UseNvarchar2}
	stmts, err := m.writer.Dialect().CreateTableStatements(m.cfg.TargetSchema, info, m.reader.DBType(), opt, m.objName)
	if err != nil {
		return "", err
	}
	return dialect.JoinSQL(stmts), nil
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
		cols, colTypes, rows, err := m.reader.ReadPage(ctx, table, pks, offset, int64(m.cfg.PageSize))
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
		if err := m.writer.CopyData(ctx, dstTable, dstCols, colTypes, rows); err != nil {
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
	cols, colTypes, firstRows, err := m.reader.ReadPage(ctx, table, pks, 0, pageSize)
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
		if err := m.writer.CopyData(ctx, dstTable, dstCols, colTypes, rows); err != nil {
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
			_, _, rows, err := m.reader.ReadPage(ctx, table, pks, off, pageSize)
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
			ddl := dialect.JoinSQL(m.writer.Dialect().IndexStatements(m.cfg.TargetSchema, pkCopy))
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
			ddl := dialect.JoinSQL(m.writer.Dialect().SequenceStatements(m.cfg.TargetSchema, seqCopy))
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
			ddl := dialect.JoinSQL(m.writer.Dialect().IndexStatements(m.cfg.TargetSchema, idxCopy))
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
				ddl := dialect.JoinSQL(m.writer.Dialect().ForeignKeyStatements(m.cfg.TargetSchema, fkCopy))
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
				vCopy.Definition = m.writer.Dialect().AdjustViewDefinition(v.Definition)
				if v.ViewName == "view_huiyuanhistorychange" {
					vCopy.Definition = `select huiyuan_histroyofchange.danweiguid AS DanWeiGuid,huiyuan_histroyofchange.pguid AS PGuid,huiyuan_histroyofchange.pname AS PName,huiyuan_histroyofchange.beforeinfo AS BeforeInfo,huiyuan_histroyofchange.changercode AS ChangerCode,huiyuan_histroyofchange.afterinfo AS AfterInfo,huiyuan_histroyofchange.changename AS ChangeName,huiyuan_histroyofchange.belongxiaqucode AS BelongXiaQuCode,huiyuan_histroyofchange.operateusername AS OperateUserName,huiyuan_histroyofchange.operatedate AS OperateDate,huiyuan_histroyofchange.row_id AS Row_ID,huiyuan_histroyofchange.danweitype AS DanWeiType,huiyuan_histroyofchange.changetype AS ChangeType,huiyuan_histroyofchange.changedate AS ChangeDate,huiyuan_histroyofchange.xiaqucode AS XiaQuCode,huiyuan_histroyofchange.viewurl AS ViewUrl,huiyuan_histroyofchange.status AS STATUS,huiyuan_histroyofchange.infoguid AS InfoGuid,huiyuan_histroyofchange.copystatus AS CopyStatus,huiyuan_histroyofchange.changecontent AS ChangeContent,huiyuan_histroyofchange.tableid AS TableID,huiyuan_histroyofchange.fieldid AS FieldID,table_struct.bak2 AS Bak2,table_struct.bak1 AS Bak1,table_struct.bak3 AS Bak3,huiyuan_histroyofchange.rowguid AS ROWGUID FROM huiyuan_histroyofchange INNER JOIN table_struct ON (((huiyuan_histroyofchange.fieldid = table_struct.fieldid::varchar) and (huiyuan_histroyofchange.tableid = table_struct.tableid::varchar)))`
				}
				if v.ViewName == "view_pwprojectinfo" {
					vCopy.Definition = `SELECT yytz_project_pw.pingweiguid AS pingweiguid, yytz_project_pw.row_id AS row_id, pw_lib.pw_name AS pwname, pw_lib.shengfenzh AS shengfenzh, pw_lib.mobile AS mobile, pw_lib.operateusername AS operateusername, pw_lib.danweiguid AS danweiguid, SUM(CASE WHEN yytz_project_pw.feedbackstatus = '可以参加' THEN 1 ELSE 0 END) AS pscount, COUNT(1) AS allcount FROM yytz_project_pw INNER JOIN pw_lib ON yytz_project_pw.pingweiguid = pw_lib.pingweiguid WHERE yytz_project_pw.projectguid IN (SELECT yytz_project.projectguid FROM yytz_project WHERE yytz_project.auditstatus = '3') GROUP BY yytz_project_pw.pingweiguid, yytz_project_pw.row_id, pw_lib.pw_name, pw_lib.shengfenzh, pw_lib.mobile, pw_lib.operateusername, pw_lib.danweiguid`
				}
				if v.ViewName == "view_pwprojectinfo_history" {
					vCopy.Definition = `SELECT yytz_project_pw.pingweiguid AS pingweiguid, yytz_project_pw.row_id AS row_id, pw_lib_history.pw_name AS pwname, pw_lib_history.shengfenzh AS shengfenzh, pw_lib_history.mobile AS mobile, pw_lib_history.operateusername AS operateusername, pw_lib_history.danweiguid AS danweiguid, SUM(CASE WHEN yytz_project_pw.feedbackstatus = '确认参加' THEN 1 ELSE 0 END) AS pscount, COUNT(1) AS allcount FROM yytz_project_pw INNER JOIN pw_lib_history ON yytz_project_pw.pingweiguid = pw_lib_history.pingweiguid WHERE yytz_project_pw.projectguid IN (SELECT yytz_project.projectguid FROM yytz_project WHERE yytz_project.auditstatus = '3') GROUP BY yytz_project_pw.pingweiguid, yytz_project_pw.row_id, pw_lib_history.pw_name, pw_lib_history.shengfenzh, pw_lib_history.mobile, pw_lib_history.operateusername, pw_lib_history.danweiguid`
				}
				if v.ViewName == "view_sys_changdinew" {
					vCopy.Definition = `SELECT mtr_project.rowguid AS RowGuid, mtr_usetime.row_id AS Row_ID, mtr_project.yudingtitle AS YuDingTitle, mtr_usetime.usedate::varchar AS UseDate, mtr_usetime.usefromhour::varchar AS UseFromHour, mtr_project.xiaqucode AS XiaQuCode, mtr_project.statuscode AS StatusCode, mtr_project.projecttype AS ProjectType, mtr_usetime.usetohour::varchar AS UseToHour, mtr_project.yudingguid AS YuDingGuid, mtr_project.showkaibiao AS ShowKaiBiao, mtr_project.showpingbiao AS ShowPingBiao, mtr_project.showbiaoduanname AS ShowBiaoDuanName, mtr_project.showbiaoduanno AS ShowBiaoDuanNo, mtr_usetime.showfromhour AS ShowFromHour, mtr_usetime.showtohour AS ShowToHour, mtr_usetime.usestep AS UseStep, mtr_usetime.rowguid AS TimeRowGuid, mtr_usetime.mtr_guid AS MTR_Guid, mtr_usetime.usestep_minute AS UseStep_Minute, mtr_project.showusedate::varchar AS ShowUseDate, mtr_usetime.usetype AS MTR_type FROM mtr_project INNER JOIN mtr_usetime ON mtr_usetime.yudingguid = mtr_project.yudingguid UNION ALL SELECT mtr_usetimehistory.rowguid AS ROWGUID, NULL, mtr_usetimehistory.yudingtitle_new AS YUDINGTITLE_NEW, NULL, mtr_usetimehistory.showusedate_new::varchar AS SHOWUSEDATE_NEW, mtr_usetimehistory.xiaqucode AS XIAQUCODE, '2', '0', mtr_usetimehistory.showpbuesdate_new AS SHOWPBUESDATE_NEW, 'history', NULL, NULL, mtr_usetimehistory.showkaibiaoguid_new AS SHOWKAIBIAOGUID_NEW, mtr_usetimehistory.showpinbiaoguid_new AS SHOWPINBIAOGUID_NEW, NULL, NULL, NULL, mtr_usetimehistory.usestep_new AS USESTEP_NEW, mtr_usetimehistory.pbusestep_new AS PBUSESTEP_NEW, NULL, NULL, NULL FROM mtr_usetimehistory WHERE mtr_usetimehistory.auditstatus <> '3'`
				}
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
