package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/target"
)

func (m *Migrator) damengBusinessFunctionsRequested() bool {
	return m.cfg.MigrationPurpose != "cdc" &&
		!m.cfg.objectMode() && len(m.cfg.ViewNames) == 0 &&
		(m.cfg.Content == "" || m.cfg.Content == "both" || m.cfg.Content == "schema_only") && m.writer.DBType() == "dameng"
}

func (m *Migrator) finishDamengBusinessFunctions(ctx context.Context, report *MigrationReport) {
	if !m.mainMigrationFinished || ctx.Err() != nil || !m.damengBusinessFunctionsRequested() {
		return
	}
	defer func() { m.compatibilityFunctions = report.CompatibilityFunctions }()
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
	// Native source identities may differ from their exact destination names.
	// Never fold quoted native names: Frame_Ou and FRAME_OU are distinct tables.
	nativeNames := map[string]string{}
	if native := report.NativeSchema; native != nil && native.Preflight != nil && native.Preflight.Snapshot != nil {
		for _, table := range native.Preflight.Snapshot.Tables {
			nativeNames[nativeSourceTable(table)] = table.Name
		}
	}
	targetName := func(name string) string {
		if report.NativeSchema != nil {
			if mapped, ok := nativeNames[name]; ok {
				return mapped
			}
			return name
		}
		return strings.ToUpper(m.objName(name)) // DaMengDialect quotes uppercase names.
	}
	m.tableState.mu.RLock()
	for table, state := range m.tableState.failures {
		blocked[targetName(table)] = state.err.Error()
	}
	m.tableState.mu.RUnlock()
	if s := m.partitionState; s != nil {
		for name, table := range s.tables {
			if s.blocked[name] {
				blocked[targetName(table.TargetTable)] = table.Error
			}
		}
	}
	m.log.Info("=== 创建达梦业务函数 ===")
	result, err := initializer.EnsureMySQLDamengBusinessFunctions(ctx, blocked)
	report.CompatibilityFunctions = result
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
