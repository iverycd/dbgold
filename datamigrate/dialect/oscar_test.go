package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
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

func TestOscarIndexNamesAndSequenceNames(t *testing.T) {
	d := NewOscar()
	for _, table := range []string{"DZT_ZNGW3_HTXZXX", "DZT_ZNGW3_HTTSCVER", "中文表"} {
		for _, unique := range []bool{false, true} {
			idx := source.IndexInfo{TableName: table, IndexName: "IDX_XZLXGUID", Columns: []string{"XZLXGUID", "SECOND"}, IsUnique: unique}
			name := IndexName(d, idx)
			require.Equal(t, table+"_IDX_XZLXGUID", name)
			sql := JoinSQL(d.IndexStatements("MixedSchema", idx))
			require.Contains(t, sql, `INDEX "`+name+`" ON "MixedSchema"."`+table+`" ("XZLXGUID", "SECOND")`)
			require.Equal(t, unique, strings.Contains(sql, "UNIQUE INDEX"))
			require.NotContains(t, sql, "IF NOT EXISTS")
			require.Equal(t, "IDX_XZLXGUID", idx.IndexName)
		}
	}
	pk := source.IndexInfo{TableName: "T", IndexName: "PRIMARY", Columns: []string{"ID"}, IsPrimary: true}
	require.Equal(t, "PK_T", IndexName(d, pk))
	require.Equal(t, `ALTER TABLE "MixedSchema"."T" ADD CONSTRAINT "PK_T" PRIMARY KEY ("ID")`, JoinSQL(d.IndexStatements("MixedSchema", pk)))
	seq := source.SequenceInfo{TableName: "T", ColumnName: "ID", StartValue: 12}
	require.Equal(t, "SEQ_T_ID", SequenceName(d, seq))
	ddl := JoinSQL(d.SequenceStatements("MixedSchema", seq, ""))
	require.Contains(t, ddl, `CREATE SEQUENCE "MixedSchema"."SEQ_T_ID"`)
	require.Contains(t, ddl, `SET DEFAULT nextval('"MixedSchema"."SEQ_T_ID"')`)
	long := source.IndexInfo{TableName: strings.Repeat("T", 200), IndexName: "IDX", Columns: []string{"X"}}
	require.Contains(t, JoinSQL(d.IndexStatements("S", long)), `"`+long.TableName+`_IDX"`)
	require.Equal(t, "seq_T_ID", SequenceName(NewPostgres("postgres"), seq))
}

func TestOscarCapabilities(t *testing.T) {
	caps := NewOscar().Caps()
	if !caps.UsesSequences || caps.SupportsDistribute || !caps.SupportsChangeOwner {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}
