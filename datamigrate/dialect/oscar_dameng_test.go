package dialect

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestOscarDamengDDLKeepsCheckedDefaultsAndIdentity(t *testing.T) {
	name := func(s string) string { return s }
	label := `'字''值'::character varying`
	bits := `B'00101'`
	flag := `true`
	info := &source.TableDDLInfo{TableName: "MixedTable", Columns: []source.ColumnInfo{
		{Name: "Id", DataType: "SERIAL", Extra: "auto_increment", IdentityInfo: &source.IdentityInfo{Seed: 33, Increment: 2}},
		{Name: "Label", DataType: "VARCHAR", Length: 25, IsNullable: true, Default: &label},
		{Name: "Bits", DataType: "BIT", Length: 5, IsNullable: true, Default: &bits},
		{Name: "Flag", DataType: "BOOL", IsNullable: true, Default: &flag},
	}}
	d := NewDaMeng()
	stmts, err := d.CreateTableStatements("app", info, "oscar", TypeOpt{}, name)
	require.NoError(t, err)
	require.Equal(t, `DROP TABLE "APP"."MIXEDTABLE" CASCADE CONSTRAINTS`, stmts[0].SQL)
	require.Contains(t, stmts[1].SQL, `"ID" INT IDENTITY(33, 2) NOT NULL`)
	require.Contains(t, stmts[1].SQL, `"LABEL" VARCHAR(25 CHAR) DEFAULT '字''值'`)
	require.Contains(t, stmts[1].SQL, `"BITS" BINARY(5) DEFAULT 0x0000010001`)
	require.Contains(t, stmts[1].SQL, `"FLAG" BIT DEFAULT 1`)
	for _, bad := range []string{`custom_function()`, `'hello'::varchar(1)`, `'x'::some_schema.varchar`} {
		label = bad
		stmts, err = d.CreateTableStatements("app", info, "oscar", TypeOpt{}, name)
		require.ErrorContains(t, err, "MixedTable.Label")
		require.Empty(t, stmts)
	}
}

func TestOscarDamengForeignKeyActions(t *testing.T) {
	d := NewDaMeng()
	fk := source.FKInfo{TableName: "Child", ConstraintName: "FkChild", Columns: []string{"Ref"}, RefTable: "Parent", RefColumns: []string{"Id"}, OnUpdate: "RESTRICT", OnDelete: "RESTRICT"}
	require.NoError(t, ValidateOscarDamengForeignKey(fk))
	require.Equal(t, `ALTER TABLE "APP"."CHILD" ADD CONSTRAINT "FKCHILD" FOREIGN KEY ("REF") REFERENCES "APP"."PARENT" ("ID")`, OscarDamengForeignKeyStatements(d, "app", fk)[0].SQL)
	fk.OnDelete = "CASCADE"
	require.Contains(t, OscarDamengForeignKeyStatements(d, "app", fk)[0].SQL, "ON DELETE CASCADE")
	fk.OnUpdate = "SET NULL"
	require.ErrorContains(t, ValidateOscarDamengForeignKey(fk), "ON UPDATE SET NULL")
	require.Empty(t, OscarDamengForeignKeyStatements(d, "app", fk))
}
