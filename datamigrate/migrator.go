// datamigrate/migrator.go
package datamigrate

import (
	"context"
	"fmt"
	"regexp"
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
	Distributed        bool     // 分布式数据库：建主键前先执行 DISTRIBUTE BY hash
	TargetSchema       string   // 目标库 schema，为空时使用连接默认 search_path
	ChangeOwner        bool     // 迁移后将表/视图/序列的 owner 改为 TargetSchema
	TargetDBType       string   // "postgres" | "gaussdb" | "seabox"
	StripViewSchemas   []string // 需从视图定义中剥离的模式名前缀(忽略大小写)
	// Objects 为"仅对象迁移"模式的对象类型白名单,取值:
	// "primary_keys" | "indexes" | "sequences" | "foreign_keys" | "comments"。
	// 为空时保持完整迁移流程(建表+数据+全部 Post-DDL);非空时只执行所列对象类型的 Post-DDL,
	// 跳过建表、数据迁移、行数对比。
	Objects []string
	// TableNames 精确表名列表(源库原始大小写)。仅对象迁移模式下作为迁移表集合,
	// 与 ListTables 结果取交集;为空则回退到 Mode+Filter 过滤。
	TableNames []string
}

// objectMode 判断是否为"仅对象迁移"模式(只补主键/索引/序列/外键,不建表不迁数据)
func (c Config) objectMode() bool {
	return len(c.Objects) > 0
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

// stripViewSchemas 从视图定义中去除用户指定的模式名前缀(忽略大小写)。
// 用于视图跨库引用其他库的表时,目标库找不到该 schema 的场景。
func (m *Migrator) stripViewSchemas(def string) string {
	for _, s := range m.cfg.StripViewSchemas {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// schema 名可能含点号(如 financeplatform_3.0),用 QuoteMeta 转义;
		// (?i) 忽略大小写;匹配 "<schema>." 整体前缀
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(s) + `\.`)
		def = re.ReplaceAllString(def, "")
	}
	return def
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
	var tables []string
	if m.cfg.objectMode() && len(m.cfg.TableNames) > 0 {
		// 仅对象模式且指定了精确表名:与源库实际表取交集,过滤脏数据
		allSet := make(map[string]bool, len(allTables))
		for _, t := range allTables {
			allSet[t] = true
		}
		for _, t := range m.cfg.TableNames {
			if allSet[t] {
				tables = append(tables, t)
			}
		}
	} else {
		tables = FilterTables(allTables, m.cfg.Mode, m.cfg.Filter)
	}

	// 仅对象迁移模式:跳过建表、数据迁移、行数对比,只执行 Post-DDL
	if m.cfg.objectMode() {
		m.log.Infof("开始对象迁移任务,共 %d 张表,对象类型: %s", len(tables), strings.Join(m.cfg.Objects, ", "))
		report.Tables.Total = len(tables)
		m.log.Info("=== 创建对象（主键/序列/索引/外键）===")
		m.createPostDDL(ctx, &report, tables, allTables)
		elapsed := time.Since(start).Round(time.Second)
		m.log.Donef("对象迁移完成,耗时 %s", elapsed)
		return report
	}

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

	// want 判断某类对象是否需要迁移:
	// 非对象模式(Objects 为空)→ 全部迁移,保持原行为;
	// 对象模式 → 仅迁移 Objects 白名单中的类型。
	want := func(objType string) bool {
		if !m.cfg.objectMode() {
			return true
		}
		for _, o := range m.cfg.Objects {
			if o == objType {
				return true
			}
		}
		return false
	}

	if want("primary_keys") {
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
	}

	if want("sequences") {
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
	}

	if want("indexes") {
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
	}

	// 外键:
	// - 非对象模式:沿用原逻辑,include 模式跳过(引用表可能不在迁移范围内)
	// - 对象模式:用户明确要求迁外键(want),引用表视为已存在,不受 include 限制
	migrateFK := want("foreign_keys") && (m.cfg.objectMode() || m.cfg.Mode != "include")
	if migrateFK {
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

	// 视图:对象模式不处理(不在对象迁移范围内);非对象模式沿用原逻辑,include 模式跳过
	if !m.cfg.objectMode() && m.cfg.Mode != "include" {
		views, err := m.reader.GetViews(ctx)
		if err != nil {
			m.log.Errorf("获取视图信息失败: %v", err)
		} else {
			report.Views.Total = len(views)
			for _, v := range views {
				if ctx.Err() != nil {
					return
				}
				ddl, cerr := m.createOneView(ctx, v)
				if cerr != nil {
					report.Views.Failed++
					report.Views.Items = append(report.Views.Items, ObjectResult{
						Name:  m.objName(v.ViewName),
						DDL:   ddl,
						Error: cerr.Error(),
					})
				} else {
					report.Views.Success++
				}
			}
		}
	}

	// 注释(表注释 + 列注释)。注意:MySQL 目标库的列注释暂不支持(需列类型重建列定义),
	// 此类语句 CommentStatements 返回空,这里跳过、不计入 Total/Failed。
	if want("comments") {
		comments, err := m.reader.GetComments(ctx)
		if err != nil {
			m.log.Errorf("获取注释信息失败: %v", err)
		} else {
			for _, cm := range comments {
				if ctx.Err() != nil {
					return
				}
				if !tableSet[cm.TableName] {
					continue
				}
				cmCopy := cm
				cmCopy.TableName = m.objName(cm.TableName)
				if cm.ColumnName != "" {
					cmCopy.ColumnName = m.objName(cm.ColumnName)
				}
				ddl := dialect.JoinSQL(m.writer.Dialect().CommentStatements(m.cfg.TargetSchema, cmCopy))
				if ddl == "" {
					// 目标库不支持该类注释(如 MySQL 列注释),跳过不计数
					continue
				}
				name := cmCopy.TableName
				if cmCopy.ColumnName != "" {
					name = fmt.Sprintf("%s.%s", cmCopy.TableName, cmCopy.ColumnName)
				}
				report.Comments.Total++
				if err := m.writer.CreateComment(ctx, cmCopy); err != nil {
					m.log.Errorf("创建注释失败 [%s]: %v", name, err)
					report.Comments.Failed++
					report.Comments.Items = append(report.Comments.Items, ObjectResult{
						Name:  name,
						DDL:   ddl,
						Error: err.Error(),
					})
				} else {
					m.log.DDLf("创建注释 %s ... OK", name)
					report.Comments.Success++
				}
			}
		}
	}
}

// createOneView 处理单个视图:对象名规范化、定义调整、特殊视图修正、剥离跨库模式名,
// 然后在目标库创建视图并按需修改 owner。返回实际执行的 DDL 与执行错误(nil 表示成功)。
func (m *Migrator) createOneView(ctx context.Context, v source.ViewInfo) (string, error) {
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
	// 去除用户指定的跨库模式名前缀(忽略大小写)
	vCopy.Definition = m.stripViewSchemas(vCopy.Definition)
	// 拼出完整 DDL 用于报告展示，与 writer.CreateView 实际执行的语句一致
	viewDDL := fmt.Sprintf("CREATE OR REPLACE VIEW \"%s\" AS\n%s;", vCopy.ViewName, vCopy.Definition)
	if m.cfg.TargetSchema != "" {
		viewDDL = fmt.Sprintf("CREATE OR REPLACE VIEW \"%s\".\"%s\" AS\n%s;", m.cfg.TargetSchema, vCopy.ViewName, vCopy.Definition)
	}
	if err := m.writer.CreateView(ctx, vCopy); err != nil {
		m.log.Errorf("创建视图失败 [%s]: %v", vCopy.ViewName, err)
		return viewDDL, err
	}
	m.log.DDLf("创建视图 %s ... OK", vCopy.ViewName)
	if m.cfg.ChangeOwner && m.cfg.TargetSchema != "" {
		if err := m.writer.ChangeOwner(ctx, "VIEW", vCopy.ViewName, m.cfg.TargetSchema); err != nil {
			m.log.Warnf("修改视图 owner 失败 [%s]: %v", vCopy.ViewName, err)
		}
	}
	return viewDDL, nil
}

// MigrateViews 按名称批量创建视图,返回每个视图的结果(复用 createOneView)。
// viewNames 以源库原始大小写匹配 ViewInfo.ViewName。
func (m *Migrator) MigrateViews(ctx context.Context, viewNames []string) []ObjectResult {
	want := make(map[string]bool, len(viewNames))
	for _, n := range viewNames {
		want[n] = true
	}
	views, err := m.reader.GetViews(ctx)
	if err != nil {
		m.log.Errorf("获取视图信息失败: %v", err)
		return nil
	}
	results := make([]ObjectResult, 0, len(viewNames))
	for _, v := range views {
		if !want[v.ViewName] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		ddl, cerr := m.createOneView(ctx, v)
		res := ObjectResult{Name: m.objName(v.ViewName), DDL: ddl}
		if cerr != nil {
			res.Error = cerr.Error()
		}
		results = append(results, res)
	}
	return results
}
