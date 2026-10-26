package handler

import (
	"testing"

	"dbgold/datamigrate/mysqldmtrigger"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengTriggerRepairScopeIsImmutable(t *testing.T) {
	src := source.TriggerInfo{Schema: "Source", Name: "BI", TableName: "Orders", Timing: "BEFORE", Event: "INSERT", Body: "SET NEW.Value = NEW.Value + 1", SourceDDL: "CREATE TRIGGER BI BEFORE INSERT ON Orders FOR EACH ROW SET NEW.Value=NEW.Value+1"}
	table := &source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Value", DataType: "int"}}}
	p, err := mysqldmtrigger.Convert(src, map[string]*source.TableDDLInfo{"Orders": table}, mysqldmtrigger.Options{Schema: "APP"})
	require.NoError(t, err)
	o := repair.Object{Kind: "trigger", Schema: p.Schema, Table: p.Table, TargetName: p.Name, MySQLDamengTrigger: &p, Status: "unknown", Units: []repair.Unit{{ID: "whole", SQL: p.TriggerSQL, OriginalSQL: p.TriggerSQL, Status: "unknown", Selected: true}}}
	require.True(t, o.CanExecuteDDL())
	require.False(t, o.CanEditDDL())
	require.NoError(t, validateRepairObjectScope(o, o.Units, "dameng"), "unknown plans require catalog reconciliation before any CREATE in the target executor")
	require.Error(t, validateRepairObjectScope(o, o.Units, "postgres"))
	edited := append([]repair.Unit(nil), o.Units...)
	edited[0].SQL += "\nNULL;"
	require.Error(t, validateRepairObjectScope(o, edited, "dameng"))
	o.MySQLDamengTrigger = nil
	require.Error(t, validateRepairObjectScope(o, o.Units, "dameng"), "a plain CREATE TRIGGER cannot acquire generated-plan execution authority")
}
