package handler

import (
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengTriggerRepairPreservesSavedConfig(t *testing.T) {
	job := &store.DataMigrationJob{SrcDBType: "mysql", DstDBType: "dameng"}
	for _, enabled := range []bool{false, true} {
		cfg := datamigrate.Config{Content: "both", MigrateTriggers: enabled}
		require.NoError(t, validateRepairTriggerConfig(job, cfg))
		require.Equal(t, enabled, cfg.MigrateTriggers)
	}
	require.NoError(t, validateRepairTriggerConfig(job, datamigrate.Config{Content: "schema_only", Objects: []string{"triggers"}}))
	require.ErrorContains(t, validateRepairTriggerConfig(job, datamigrate.Config{Content: "data_only", MigrateTriggers: true}), "仅迁移数据")
}

func TestMySQLDamengTriggerRepairDependencyDataAndMapping(t *testing.T) {
	plan := mysqldmtrigger.Plan{Schema: "TARGET", Table: "ORDERS", Name: "TRG", Dependencies: []string{"ORDERS"}, SourceTables: []source.TableDDLInfo{{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}, {Name: "Value", DataType: "int"}}}}, Options: mysqldmtrigger.Options{TableNames: map[string]string{"Orders": "ORDERS"}, ColumnNames: map[string]map[string]string{"Orders": {"Id": "ID", "Value": "VALUE"}}}}
	object := repair.Object{Kind: "trigger", Schema: "TARGET", Table: "ORDERS", TargetName: "TRG", MySQLDamengTrigger: &plan, Dependencies: []string{"orders", "preceding"}}
	dep := repair.Object{ID: "orders", Kind: "table", Schema: "TARGET", Table: "ORDERS", TargetName: "ORDERS", SourceTable: "Orders", Status: "succeeded", DataStatus: "succeeded", ColumnMapping: map[string]string{"Id": "ID", "Value": "VALUE"}, Units: []repair.Unit{{Status: "succeeded"}}}
	predecessor := repair.Object{ID: "preceding", Kind: "trigger", TargetName: "BEFORE_TRG", Status: "succeeded"}
	check := func() error {
		return validateMySQLDamengTriggerDependencyObjects(object, []repair.Object{dep, predecessor})
	}
	require.NoError(t, check())
	dep.DataStatus = "failed"
	require.ErrorContains(t, check(), "数据状态")
	dep.DataStatus = "succeeded"
	dep.Status = "failed"
	require.ErrorContains(t, check(), "结构尚未成功")
	dep.Status = "succeeded"
	dep.ColumnMapping["Id"], dep.ColumnMapping["Value"] = "VALUE", "ID"
	require.ErrorContains(t, check(), "映射已改变")
	dep.ColumnMapping["Id"], dep.ColumnMapping["Value"] = "ID", "VALUE"
	predecessor.Status = "failed"
	require.ErrorContains(t, check(), "前序触发器")
	predecessor.Status = "succeeded"
	require.ErrorContains(t, validateMySQLDamengTriggerDependencyObjects(object, []repair.Object{dep}), "清单缺少")

	// Table ledger names follow the existing logical objName convention while
	// DM emits their physical uppercase spelling. Both must identify the same
	// dependency, including lower_case_names jobs.
	dep.Table, dep.TargetName, dep.Schema = "orders", "orders", "target"
	require.NoError(t, check())
	dep.DataStatus = "failed"
	require.ErrorContains(t, check(), "数据状态")
	dep.DataStatus = "succeeded"
	dep.ColumnMapping["Id"], dep.ColumnMapping["Value"] = "VALUE", "ID"
	require.ErrorContains(t, check(), "映射已改变")
	dep.ColumnMapping["Id"], dep.ColumnMapping["Value"] = "ID", "VALUE"
	object.Dependencies = nil
	require.NoError(t, validateMySQLDamengTriggerDependencyObjects(object, nil), "external target tables are checked by the executor")
}
