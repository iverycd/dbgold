package datamigrate

import (
	"context"
	"fmt"
	"slices"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/validation"
)

// The inventory is captured before table DDL. Validation must never silently
// adopt a new source selection or column set after migration has finished.
type frozenValidationTable struct {
	name     string
	target   string // optional native materialized-view storage mapping
	metadata validation.Table
	err      error
}

func (m *Migrator) dataValidationEnabled() bool {
	return m.cfg.ValidateData && m.cfg.Content != "schema_only" && !m.cfg.objectMode() && len(m.cfg.ViewNames) == 0
}

func (m *Migrator) logMigrationCompletion(format string, args ...any) {
	if m.dataValidationEnabled() {
		m.log.Infof("%s；接下来执行全量内容校验", fmt.Sprintf(format, args...))
	} else {
		m.log.Donef(format, args...)
	}
}

func validationSource(r source.Reader) (validation.Scanner, bool) {
	// The SQL Server -> Dameng metadata snapshot embeds the old Reader interface,
	// which deliberately does not include the optional validation methods.
	if snapshot, ok := r.(*sqlServerDamengSnapshot); ok {
		return validationSource(snapshot.Reader)
	}
	scanner, ok := r.(validation.Scanner)
	return scanner, ok
}

func (m *Migrator) freezeDataValidation(ctx context.Context, tables []string) {
	if !m.dataValidationEnabled() {
		return
	}
	m.validationScopeFrozen = true
	m.validationTables = make([]frozenValidationTable, 0, len(tables))
	scanner, supported := validationSource(m.reader)
	for _, table := range tables {
		frozen := frozenValidationTable{name: table}
		if !supported {
			frozen.err = fmt.Errorf("源库 %s 不支持原值内容读取", m.reader.DBType())
		} else {
			frozen.metadata, frozen.err = scanner.DescribeValidationTable(ctx, table)
			frozen.metadata.Columns = slices.Clone(frozen.metadata.Columns)
			frozen.metadata.PrimaryKey = slices.Clone(frozen.metadata.PrimaryKey)
		}
		m.validationTables = append(m.validationTables, frozen)
	}
}

func (m *Migrator) markDataValidationCopied(table string) {
	if m.dataValidationEnabled() {
		m.validationCopied.Store(table, true)
	}
}

func (m *Migrator) validateDataContent(ctx context.Context, report *MigrationReport) {
	result := report.DataValidation
	result.Tables = make([]validation.TableResult, 0, len(m.validationTables))
	if !m.validationScopeFrozen {
		result.Status = validation.StatusError
		result.Tables = append(result.Tables, validation.TableResult{Status: validation.StatusError, Error: "迁移准备未完成，未执行内容校验"})
		if ctx.Err() != nil {
			result.Status, result.Tables[0].Status = validation.StatusCancelled, validation.StatusCancelled
		}
		return
	}
	if len(m.validationTables) == 0 {
		result.Status = validation.StatusIncomplete
		result.Tables = append(result.Tables, validation.TableResult{Status: validation.StatusIncomplete, Error: "本次没有可校验的数据表，未执行内容校验"})
		return
	}
	m.log.Info("=== 全量数据内容校验（原值，只读）===")
	src, srcOK := validationSource(m.reader)
	dst, dstOK := m.writer.(validation.Scanner)
	for _, frozen := range m.validationTables {
		targetName := frozen.target
		if targetName == "" {
			targetName = m.objName(frozen.name)
		}
		table := validation.TableResult{SourceTable: frozen.name, TargetTable: targetName}
		switch {
		case ctx.Err() != nil:
			table.Status, table.Error = validation.StatusCancelled, "任务已取消，内容校验未完成"
		case !srcOK || !dstOK:
			table.Status, table.Error = validation.StatusUnsupported, "源库或目标库不支持原值内容读取"
		case frozen.err != nil:
			table.Status, table.Error = validation.StatusError, "冻结源列元数据失败: "+frozen.err.Error()
		default:
			if _, copied := m.validationCopied.Load(frozen.name); !copied {
				table.Status, table.Error = validation.StatusError, "该表数据迁移未完成，无法完成内容校验"
			} else {
				plan, err := m.dataValidationPlan(ctx, dst, frozen)
				if err != nil {
					table.Status, table.Error = validation.StatusError, err.Error()
				} else {
					m.log.Infof("开始内容校验 [%s]", frozen.name)
					table = validation.Validate(ctx, src, dst, plan)
				}
			}
		}
		result.Tables = append(result.Tables, table)
		if table.Status == validation.StatusPassed {
			m.log.Dataf("内容校验 [%s]: 源=%d 目标=%d ... OK", frozen.name, table.SourceRows, table.TargetRows)
		} else {
			m.log.Errorf("内容校验 [%s]: %s，缺失=%d 多余=%d 内容不同=%d %s", frozen.name, table.Status, table.MissingRows, table.ExtraRows, table.ChangedRows, table.Error)
		}
	}
	result.Finalize()
	if ctx.Err() != nil {
		m.log.Warn("内容校验已取消，已迁移数据保留")
	} else {
		m.log.Donef("迁移与内容校验结束：内容校验状态 %s，已检查 %d 张表", result.Status, len(result.Tables))
	}
}

func (m *Migrator) dataValidationPlan(ctx context.Context, dst validation.Scanner, frozen frozenValidationTable) (validation.Plan, error) {
	targetName := frozen.target
	if targetName == "" {
		targetName = m.objName(frozen.name)
	}
	plan := validation.Plan{SourceTable: frozen.name, TargetTable: targetName, SourceColumns: frozen.metadata.Columns, PrimaryKey: frozen.metadata.PrimaryKey}
	if len(plan.SourceColumns) == 0 {
		return plan, fmt.Errorf("冻结源表定义没有可校验列")
	}
	metadata, err := dst.DescribeValidationTable(ctx, plan.TargetTable)
	if err != nil {
		return plan, fmt.Errorf("读取目标列元数据失败: %w", err)
	}
	if len(metadata.Columns) != len(plan.SourceColumns) {
		return plan, fmt.Errorf("源目标列数量不同: 源=%d 目标=%d", len(plan.SourceColumns), len(metadata.Columns))
	}
	actual := make(map[string]validation.Column, len(metadata.Columns))
	for _, column := range metadata.Columns {
		if _, exists := actual[column.Name]; exists {
			return plan, fmt.Errorf("目标列名重复: %s", column.Name)
		}
		actual[column.Name] = column
	}
	for _, column := range plan.SourceColumns {
		name := m.targetColumn(frozen.name, column.Name)
		mapped, exists := actual[name]
		if !exists {
			return plan, fmt.Errorf("目标列映射不存在: %s → %s", column.Name, name)
		}
		plan.TargetColumns = append(plan.TargetColumns, mapped)
		delete(actual, name)
	}
	if len(actual) > 0 {
		return plan, fmt.Errorf("目标存在未映射列")
	}
	return plan, nil
}
