package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"dbgold/datamigrate/valueconv"
)

type oscarTableStream interface {
	StreamOscarTable(context.Context, string, int, func([]string, []string, [][]any) error) error
}

type oscarMySQLWriter interface {
	ConfigureOscarMigration(context.Context) error
	MySQLTableNameKey(string) string
	OscarMySQLVersion() string
	ValidateOscarMySQLIdentity(context.Context, source.SequenceInfo, string) error
	ValidateOscarMySQLAutoIncrement(context.Context, source.SequenceInfo, string, bool) error
	PrepareOscarMySQLAutoIncrement(context.Context, source.SequenceInfo) ([]dialect.Statement, string, error)
	ExecuteOscarMySQLAutoIncrement(context.Context, source.SequenceInfo, []dialect.Statement, string) error
}

func (m *Migrator) isOscarMySQL() bool {
	return m.reader.DBType() == "oscar" && m.writer.DBType() == "mysql"
}

func (m *Migrator) migrationTargetNameKey(name string) string {
	if m.isOscarMySQL() {
		if w, ok := m.writer.(oscarMySQLWriter); ok {
			return w.MySQLTableNameKey(name)
		}
	}
	return name
}

func configureOscarMySQL(ctx context.Context, w target.Writer) (oscarMySQLWriter, error) {
	writer, ok := w.(oscarMySQLWriter)
	if !ok {
		return nil, fmt.Errorf("目标缺少神通到 MySQL 的版本和会话适配")
	}
	if err := writer.ConfigureOscarMigration(ctx); err != nil {
		return nil, err
	}
	return writer, nil
}

func (m *Migrator) preflightOscarMySQLTables(ctx context.Context, tables []string, report *MigrationReport) {
	w := m.writer.(oscarMySQLWriter)
	appendData := m.cfg.Content == "data_only" && !m.cfg.objectMode()
	if appendData {
		m.oscarAppendCounts = make(map[string]int64)
	}
	var sequences []source.SequenceInfo
	var sequenceErr error
	readSequences := false
	for _, table := range tables {
		if m.migrationTableBlocked(table) {
			continue
		}
		info, err := m.sourceTableDDLInfo(ctx, table)
		if err == nil && info != nil {
			for _, col := range info.Columns {
				if !strings.EqualFold(col.Extra, "auto_increment") {
					continue
				}
				if !readSequences {
					sequences, sequenceErr = m.reader.GetSequences(ctx)
					readSequences = true
				}
				if sequenceErr != nil {
					err = sequenceErr
					break
				}
				var seq source.SequenceInfo
				for _, candidate := range sequences {
					if candidate.TableName == table && candidate.ColumnName == col.Name {
						seq = m.targetSequence(candidate)
						break
					}
				}
				if seq.ColumnName == "" {
					err = fmt.Errorf("源列 %s 的只读自增续号证据缺失", col.Name)
					break
				}
				typ := m.writer.Dialect().MapType(col, "oscar", dialect.TypeOpt{})
				if m.cfg.Content == "data_only" || m.cfg.objectMode() {
					err = w.ValidateOscarMySQLAutoIncrement(ctx, seq, typ, !m.cfg.objectMode())
				} else {
					err = w.ValidateOscarMySQLIdentity(ctx, seq, typ)
				}
				if err != nil {
					break
				}
			}
		}
		if err == nil && appendData && !m.hasPartitionPlan(table) {
			count, e := m.writer.CountRows(ctx, m.objName(table))
			if e != nil {
				err = fmt.Errorf("冻结目标表追加前行数: %w", e)
				report.addDataIssue(table, m.objName(table), rowCountError, err.Error())
			} else {
				m.oscarAppendCounts[table] = count
			}
		}
		if err != nil {
			if m.stopMigrationTask(ctx, err) {
				return
			}
			m.blockMigrationTable(table, "preflight", err)
		}
	}
}

func (m *Migrator) warnOscarMySQLSequenceRestart(report *MigrationReport, seq source.SequenceInfo) {
	if strings.HasPrefix(m.writer.(oscarMySQLWriter).OscarMySQLVersion(), "5.7.") {
		report.Warnings = append(report.Warnings, valueconv.MigrationWarning{Table: seq.TableName, Column: seq.ColumnName, Count: 1, Reason: "MySQL 5.7 重启后可能按表内最大 ID 重建自增计数，未入表的号段可能被重用"})
	}
}

func (m *Migrator) recordOscarManualObjects(ctx context.Context, report *MigrationReport) {
	if m.cfg.Content == "data_only" || m.cfg.Mode == "include" || m.cfg.objectMode() {
		return
	}
	r, ok := m.reader.(interface {
		OscarManualObjects(context.Context) ([]source.OscarManualObject, error)
	})
	if !ok {
		return
	}
	objects, err := r.OscarManualObjects(ctx)
	if m.stopMigrationTask(ctx, err) {
		return
	}
	if err != nil {
		report.Warnings = append(report.Warnings, valueconv.MigrationWarning{Table: m.dmSourceSchema, Column: "对象盘点", Count: 1, Reason: err.Error()})
	}
	labels := map[string]string{"routine": "函数或存储过程", "trigger": "触发器", "sequence": "独立或共享序列"}
	for _, object := range objects {
		label := labels[object.Kind]
		report.Warnings = append(report.Warnings, valueconv.MigrationWarning{Table: m.dmSourceSchema, Column: object.Name, Count: 1, Reason: label + "需在 MySQL 中手工适配"})
	}
}

func (m *Migrator) oscarSourceTableDDL(ctx context.Context, table string) string {
	r, ok := m.reader.(interface {
		GetTableSourceDDL(context.Context, string) (string, error)
	})
	if !ok {
		return ""
	}
	ddl, err := r.GetTableSourceDDL(ctx, table)
	m.stopMigrationTask(ctx, err)
	return ddl
}

func (m *Migrator) migrateOscarMySQLTable(ctx context.Context, table string) error {
	if m.cfg.Content == "data_only" {
		if err := m.oscarAppendReady(ctx, table); err != nil {
			m.stopMigrationTask(ctx, err)
			return err
		}
	}
	r, ok := m.reader.(oscarTableStream)
	if !ok {
		return fmt.Errorf("神通源库缺少固定游标读取能力")
	}
	var copied int64
	err := r.StreamOscarTable(ctx, table, m.cfg.PageSize, func(cols, types []string, rows [][]any) error {
		mapped := make([]string, len(cols))
		for i, col := range cols {
			mapped[i] = m.targetColumn(table, col)
		}
		if err := m.copyTableBatch(ctx, m.objName(table), mapped, types, rows); err != nil {
			return fmt.Errorf("已写入 %d 行后复制失败: %w", copied, err)
		}
		copied += int64(len(rows))
		return nil
	})
	if err != nil {
		m.stopMigrationTask(ctx, err)
		m.log.Errorf("迁移神通源表 [%s] 失败: %v", table, err)
		return err
	}
	m.log.Dataf("迁移 %s: %d 行 ... OK", table, copied)
	return nil
}

func (m *Migrator) freezeOscarAppendContent(ctx context.Context, frozen *frozenValidationTable) {
	count, appendData := m.oscarAppendCounts[frozen.name]
	if !appendData || count == 0 {
		return
	}
	dst, ok := m.writer.(validation.Scanner)
	if !ok {
		frozen.err = fmt.Errorf("目标不支持追加前原值基线读取")
		return
	}
	plan, err := m.dataValidationPlan(ctx, dst, *frozen)
	if err == nil && m.isOscarDameng() {
		frozen.unsupported = oscarDamengUnverifiableColumns(plan)
		if frozen.unsupported != "" {
			return
		}
	}
	if err == nil {
		frozen.appendBaseline, err = validation.CaptureAppendBaseline(ctx, dst, plan)
	}
	if err != nil {
		frozen.err = fmt.Errorf("冻结目标表追加前原值: %w", err)
	} else if frozen.appendBaseline.Rows() != count {
		frozen.err = fmt.Errorf("目标表 %s 在冻结行数与内容基线之间已变化", frozen.name)
	}
}

func (m *Migrator) oscarAppendReady(ctx context.Context, table string) error {
	baseline, ok := m.oscarAppendCounts[table]
	if !ok {
		return dataCheckError{rowCountError, fmt.Errorf("缺少目标表 %s 的追加前行数", table)}
	}
	if m.dataValidationEnabled() {
		for _, frozen := range m.validationTables {
			if frozen.name == table && frozen.err != nil {
				return dataCheckError{verificationError, frozen.err}
			}
		}
	}
	count, err := m.writer.CountRows(ctx, m.objName(table))
	if err != nil {
		return dataCheckError{rowCountError, countReadError(nil, err)}
	}
	if count != baseline {
		return dataCheckError{verificationError, fmt.Errorf("目标表 %s 行数已偏离追加前基线", table)}
	}
	return nil
}

func recoverOscarRows(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview, truncate bool, progress func(repair.RecoveryResult) error, ops recoveryOps, result *repair.RecoveryResult) error {
	var stream func(context.Context, string, int, func([]string, []string, [][]any) error) error
	if w.DBType() == "dameng" {
		if reader, ok := r.(oscarDamengTableStream); ok {
			stream = reader.StreamOscarDamengTable
		}
	} else if reader, ok := r.(oscarTableStream); ok {
		stream = reader.StreamOscarTable
	}
	if stream == nil {
		return fmt.Errorf("神通源库缺少固定游标读取能力")
	}
	start := func() error {
		if progress != nil {
			if err := progress(*result); err != nil {
				return err
			}
		}
		if truncate {
			if err := ops.truncate(ctx, w, p.TargetTable); err != nil {
				return err
			}
			result.TargetRows = 0
		}
		return nil
	}
	started := false
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = 2000
	}
	err := stream(ctx, p.SourceTable, pageSize, func(cols, types []string, rows [][]any) error {
		if err := checkRecoveryPageColumns(p, cols); err != nil {
			return err
		}
		if !started {
			if p.SourceRows == 0 || int64(len(rows)) > p.SourceRows {
				return fmt.Errorf("源端首批行数与预览不一致，请重新预览")
			}
			if err := start(); err != nil {
				return err
			}
			started = true
		}
		dst := make([]string, len(cols))
		for i, col := range cols {
			dst[i] = p.ColumnMapping[col]
		}
		if err := w.CopyData(ctx, p.TargetTable, dst, types, rows); err != nil {
			result.Status = "unknown"
			return fmt.Errorf("写入失败，已确认提交 %d 行；禁止追加重试: %w", result.CommittedRows, err)
		}
		result.CommittedRows += int64(len(rows))
		if progress != nil {
			return progress(*result)
		}
		return nil
	})
	if err != nil || started {
		return err
	}
	if p.SourceRows != 0 {
		return fmt.Errorf("源端未返回预览中的 %d 行数据，请重新预览", p.SourceRows)
	}
	return start()
}
