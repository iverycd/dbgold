package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/target"
)

func (m *Migrator) mysqlDMBusinessFunctionsRequested() bool {
	return m.cfg.MigrationPurpose != "cdc" &&
		!m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 &&
		(m.cfg.Content == "" || m.cfg.Content == "both" || m.cfg.Content == "schema_only") && m.isMySQLDameng()
}

func (m *Migrator) finishMySQLDamengBusinessFunctions(ctx context.Context, report *MigrationReport) {
	if !m.mainMigrationFinished || ctx.Err() != nil || !m.mysqlDMBusinessFunctionsRequested() {
		return
	}
	if m.repairs != nil {
		m.repairs.mu.Lock()
		err := m.repairs.sinkErr
		m.repairs.mu.Unlock()
		if err != nil {
			m.stopMigrationTask(ctx, err)
			return
		}
	}
	initializer, ok := m.writer.(target.MySQLDamengBusinessFunctionInitializer)
	if !ok {
		report.CompatibilityFunctions = &target.CompatibilityFunctionsReport{Total: 1, Failed: 1,
			Items: []target.CompatibilityFunctionResult{{Name: "达梦业务函数", Status: "failed", Error: "目标 Writer 未实现达梦业务函数安装能力"}}}
		return
	}
	blocked := map[string]string{}
	m.tableState.mu.RLock()
	for table, state := range m.tableState.failures {
		blocked[strings.ToUpper(table)] = state.err.Error()
	}
	m.tableState.mu.RUnlock()
	if s := m.partitionState; s != nil {
		for name, table := range s.tables {
			if s.blocked[name] {
				blocked[strings.ToUpper(table.TargetTable)] = table.Error
			}
		}
	}
	m.log.Info("=== 创建 MySQL→达梦业务函数 ===")
	result, err := initializer.EnsureMySQLDamengBusinessFunctions(ctx, blocked)
	report.CompatibilityFunctions, m.compatibilityFunctions = result, result
	if result != nil {
		for _, item := range result.Items {
			if item.Status == "failed" {
				m.log.Errorf("业务函数 %s.%s: %s", item.Schema, item.Name, item.Error)
			} else if item.Status == "reused" {
				m.log.DDLf("业务函数 %s.%s: 已存在，跳过，未校验定义", item.Schema, item.Name)
			} else {
				m.log.DDLf("业务函数 %s.%s: 已创建", item.Schema, item.Name)
			}
		}
	}
	if err != nil {
		m.log.Errorf("达梦业务函数安装失败: %v", err)
		m.stopMigrationTask(ctx, err)
	}
	if result == nil {
		report.CompatibilityFunctions = &target.CompatibilityFunctionsReport{Total: 1, Failed: 1,
			Items: []target.CompatibilityFunctionResult{{Name: "达梦业务函数", Status: "failed", Error: fmt.Sprintf("未返回函数安装报告: %v", err)}}}
	}
}
