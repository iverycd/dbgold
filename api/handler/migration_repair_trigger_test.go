package handler

import (
	"strings"
	"testing"

	"dbgold/datamigrate"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/store"
	"github.com/stretchr/testify/require"
)

func TestMySQLPGTriggerRepairScopeAcceptsOnlyFrozenProgram(t *testing.T) {
	table := source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "int"}}}
	p, err := mysqltrigger.Convert(source.TriggerInfo{Schema: "Source", Name: "trg", TableName: "Orders", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1"}, map[string]*source.TableDDLInfo{"Orders": &table}, mysqltrigger.Options{Schema: "Target"})
	require.NoError(t, err)
	o := repair.Object{Kind: "trigger", Schema: p.Schema, Table: p.Table, TargetName: p.Name, MySQLPGTrigger: &p, Status: "failed", Units: []repair.Unit{{SQL: p.FunctionSQL, OriginalSQL: p.FunctionSQL, Selected: true, Status: "rolled_back"}, {SQL: p.TriggerSQL, OriginalSQL: p.TriggerSQL, Selected: true, Status: "failed"}}}
	require.NoError(t, validateRepairUnits(o.Units))
	require.NoError(t, validateRepairObjectScope(o, o.Units, "postgres"))
	changed := append([]repair.Unit(nil), o.Units...)
	changed[0].SQL += " SELECT 1;"
	require.Error(t, validateRepairObjectScope(o, changed, "postgres"))
	changed = append([]repair.Unit(nil), o.Units...)
	changed[1].Selected = false
	require.Error(t, validateRepairObjectScope(o, changed, "postgres"))
	require.Error(t, validateRepairObjectScope(o, o.Units, "mysql"))
	o.MySQLPGTrigger = nil
	require.Error(t, validateRepairObjectScope(o, o.Units, "postgres"))
}

func triggerDependencyFixture(t *testing.T, lower bool) (repair.Object, repair.Object) {
	t.Helper()
	table := source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "ID", DataType: "int"}, {Name: "Value", DataType: "int"}}}
	op := mysqltrigger.Options{Schema: "Target"}
	name := func(s string) string { return s }
	if lower {
		op.Name = strings.ToLower
		name = strings.ToLower
	}
	p, err := mysqltrigger.Convert(source.TriggerInfo{Schema: "Source", Name: "trg", TableName: "Orders", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1"}, map[string]*source.TableDDLInfo{"Orders": &table}, op)
	require.NoError(t, err)
	p.Tables = []source.TableDDLInfo{{TableName: name("Orders"), Columns: []source.ColumnInfo{{Name: name("ID"), DataType: "int"}, {Name: name("Value"), DataType: "int"}}}}
	o := repair.Object{Kind: "trigger", Schema: p.Schema, Table: p.Table, TargetName: p.Name, MySQLPGTrigger: &p, Status: "blocked", Dependencies: []string{"table-id"}}
	dep := repair.Object{ID: "table-id", Kind: "table", Schema: p.Schema, Table: p.Table, TargetName: p.Table, SourceTable: "Orders", Status: "succeeded", DataStatus: "succeeded", Columns: p.Tables[0].Columns, ColumnMapping: map[string]string{"ID": name("ID"), "Value": name("Value")}, Units: []repair.Unit{{Status: "succeeded"}}}
	return o, dep
}

func TestTriggerDependencyDataMustFinishBeforeInstalling(t *testing.T) {
	for _, status := range []string{"failed", "pending", "unknown", ""} {
		t.Run("data_"+status, func(t *testing.T) {
			o, dep := triggerDependencyFixture(t, false)
			dep.DataStatus = status
			require.ErrorContains(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}), "数据状态")
		})
	}
	o, dep := triggerDependencyFixture(t, false)
	dep.Status = "failed"
	require.ErrorContains(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}), "结构尚未成功")
	dep.Status = "succeeded"
	require.NoError(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}))
	dep.Status = "unknown"
	dep.DataStatus = "not_required"
	require.Error(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}))
	dep.Units = nil
	require.NoError(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}), "object-only existing tables are checked against the target snapshot")
}

func TestTriggerDependencyFrozenMappingCannotBeExchanged(t *testing.T) {
	for _, lower := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "lowercase"}[lower], func(t *testing.T) {
			o, dep := triggerDependencyFixture(t, lower)
			require.NoError(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}))
			dep.ColumnMapping["ID"], dep.ColumnMapping["Value"] = dep.ColumnMapping["Value"], dep.ColumnMapping["ID"]
			require.ErrorContains(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}), "映射已改变")
			delete(dep.ColumnMapping, "ID")
			require.ErrorContains(t, validateMySQLPGTriggerDependencyObjects(o, []repair.Object{dep}), "列映射已偏离")
		})
	}
}

func TestTriggerDependencyReadsPersistedRecoveryState(t *testing.T) {
	db := repairPersistenceDB(t)
	o, dep := triggerDependencyFixture(t, false)
	dep.DataStatus = "failed"
	row := store.MigrationRepairObject{JobID: "trigger-dependency", ObjectID: dep.ID, ObjectJSON: repairJSON(dep), Version: 1}
	require.NoError(t, db.Create(&row).Error)
	require.ErrorContains(t, validateMySQLPGTriggerDependencies(row.JobID, o), "数据状态")
	dep.DataStatus = "succeeded"
	require.NoError(t, db.Model(&row).Update("object_json", repairJSON(dep)).Error)
	require.NoError(t, validateMySQLPGTriggerDependencies(row.JobID, o))
	require.ErrorContains(t, validateMySQLPGTriggerDependencies("missing-job", o), "依赖清单缺少")
	o.Dependencies = nil
	require.NoError(t, validateMySQLPGTriggerDependencyObjects(o, nil), "external tables remain subject to target structure validation")
	require.NoError(t, validateMySQLPGTriggerDependencies("missing-job", repair.Object{Kind: "index"}), "generic DDL retries are unaffected")
}

func TestRepairTriggerBindingConfig(t *testing.T) {
	j := &store.DataMigrationJob{SrcDBType: "mysql", DstDBType: "postgres"}
	require.NoError(t, validateRepairTriggerConfig(j, datamigrate.Config{Content: "schema_only", Objects: []string{"triggers"}}))
	require.Error(t, validateRepairTriggerConfig(j, datamigrate.Config{Content: "data_only", MigrateTriggers: true}))
	require.Error(t, validateRepairTriggerConfig(j, datamigrate.Config{Content: "schema_only", Objects: []string{"unknown"}}))
	j.DstDBType = "mysql"
	require.Error(t, validateRepairTriggerConfig(j, datamigrate.Config{Content: "schema_only", Objects: []string{"triggers"}}))
}
