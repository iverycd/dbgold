package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func TestOscarCreateTableAndQuoting(t *testing.T) {
	zero := "0"
	info := &source.TableDDLInfo{TableName: `Order"表`, Columns: []source.ColumnInfo{
		{Name: `ID"列`, DataType: "int", IsNullable: false},
		{Name: "created", DataType: "datetime", Default: &zero, IsNullable: true},
	}}
	d := NewOscar()
	stmts, err := d.CreateTableStatements("业务", info, "mysql", TypeOpt{}, func(v string) string { return v })
	if err != nil {
		t.Fatal(err)
	}
	got := JoinSQL(stmts)
	for _, fragment := range []string{`"业务"."Order""表"`, `"ID""列" INTEGER NOT NULL`, "DEFAULT 0"} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("DDL missing %q:\n%s", fragment, got)
		}
	}
}

func TestOscarCapabilities(t *testing.T) {
	caps := NewOscar().Caps()
	if !caps.UsesSequences || caps.SupportsDistribute || !caps.SupportsChangeOwner {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}
