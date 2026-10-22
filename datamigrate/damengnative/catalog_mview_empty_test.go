package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMaterializedEmptyDefaultOnNullUsesColumnSyntaxOnly(t *testing.T) {
	got, err := materializedDefaultOnNullColumns(`CREATE TABLE "Mixed"."Base"("default on null" VARCHAR(80) DEFAULT 'DEFAULT ON NULL',"Actual" INT DEFAULT ON NULL (1+2),"(" VARCHAR(80) DEFAULT '(',"Nested" VARCHAR(80) DEFAULT concat('DEFAULT ON NULL',','),CONSTRAINT "Default On Null" CHECK("Actual">0)) STORAGE(ON "MAIN",CLUSTERBTR)`)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"Actual": true}, got)
}

func TestMaterializedEmptyTouchFreezesMetadataWithoutWrites(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "MiXeD", snapshot: &Snapshot{Objects: []Object{{Kind: "constraint", Name: "pK", Table: "EmptyBase", Generated: true}}}}
	ddl := `CREATE TABLE "MiXeD"."EmptyBase"("Id" BIGINT IDENTITY(1,2),"Text" VARCHAR(80) DEFAULT ON NULL 'MiXeD.a',CONSTRAINT "pK" PRIMARY KEY("Id"))`
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TABLE", "EmptyBase", "MiXeD").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(ddl))
	m.ExpectQuery(`SELECT COLUMN_NAME,NULLABLE,DATA_DEFAULT`).WithArgs("MiXeD", "EmptyBase").WillReturnRows(sqlmock.NewRows([]string{"column", "nullable", "default"}).AddRow("Id", "N", nil).AddRow("Text", "Y", `'MiXeD.a'`).AddRow("Generated", "Y", nil))
	m.ExpectQuery(`SELECT CONSTRAINT_NAME,TABLE_NAME,CONSTRAINT_TYPE,GENERATED,STATUS,VALIDATED.*CONSTRAINT_TYPE IN \('P','U'\)`).WithArgs("MiXeD", "EmptyBase", "MiXeD", "MiXeD", "EmptyBase").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "type", "generated", "state", "validated"}).AddRow("pK", "EmptyBase", "P", nil, "ENABLED", "NOT VALIDATED").AddRow("Fk", "Child", "R", "GENERATED NAME", "DISABLED", nil))
	m.ExpectQuery(`SELECT COLUMN_NAME FROM ALL_CONS_COLUMNS`).WithArgs("MiXeD", "EmptyBase", "pK").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Id"))
	fkDDL := `ALTER TABLE "MiXeD"."Child" ADD CONSTRAINT "Fk" FOREIGN KEY("ParentId") REFERENCES "MiXeD"."EmptyBase"("Id") ON DELETE CASCADE DISABLE`
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("CONSTRAINT", "Fk", "MiXeD").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(fkDDL))
	m.ExpectQuery(`SELECT COLUMN_NAME FROM ALL_CONS_COLUMNS`).WithArgs("MiXeD", "Child", "Fk").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("ParentId"))
	p, err := r.freezeMaterializedEmptyTouch(Table{Name: "EmptyBase", Rows: 0, Columns: []Column{{Name: "Id", Writable: true, Identity: true}, {Name: "Text", Writable: true}, {Name: "Generated", Writable: false}}, Restore: []Unit{{SQL: "frozenDrop"}, {SQL: "frozenAdd"}}})
	require.NoError(t, err)
	require.Equal(t, "EmptyBase", p.Table)
	require.Equal(t, ddl, p.DefinitionSQL)
	require.Equal(t, []MaterializedEmptyColumn{{Name: "Id", NotNull: true}, {Name: "Text", DefaultSQL: `'MiXeD.a'`, DefaultOnNull: true}}, p.Columns)
	require.Equal(t, []string{"frozenDrop", "frozenAdd"}, p.IdentityRestore)
	require.Equal(t, []MaterializedEmptyConstraint{{Name: "pK", Table: "EmptyBase", Kind: "P", Columns: []string{"Id"}, Generated: true, Enabled: true, NoValidate: true}, {Name: "Fk", Table: "Child", Kind: "R", Columns: []string{"ParentId"}, Generated: true, DefinitionSQL: fkDDL}}, p.Constraints)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestEmptyIdentityUsesSafeContinuationAfterFirstValueMayHaveBeenDeleted(t *testing.T) {
	for _, c := range []struct{ seed, step, next string }{{"1", "1", "2"}, {"10", "-3", "7"}} {
		t.Run(c.seed+"_"+c.step, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := &catalogReader{ctx: context.Background(), db: db, schema: "MiXeD", snapshot: &Snapshot{}}
			q := Qualified("MiXeD", "Empty")
			m.ExpectQuery(`SELECT CAST\(IDENT_SEED`).WithArgs(q, q, q).WillReturnRows(sqlmock.NewRows([]string{"seed", "step", "current"}).AddRow(c.seed, c.step, c.seed))
			table := Table{Name: "Empty", Rows: 0, Columns: []Column{{Name: "Id", Identity: true, Writable: true}}}
			r.identityRestore(&Object{ID: "base", Kind: "table", Name: "Empty"}, &table)
			require.Len(t, table.Restore, 2)
			require.Equal(t, `ALTER TABLE "MiXeD"."Empty" ADD COLUMN "Id" IDENTITY(`+c.next+`, `+c.step+`)`, table.Restore[1].SQL)
			require.Len(t, r.snapshot.Issues, 1)
			require.Equal(t, "warning", r.snapshot.Issues[0].Severity)
			require.Contains(t, r.snapshot.Issues[0].Reason, "可能跳过尚未使用的首值")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestMaterializedEmptyTouchRejectsMissingColumnMetadata(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App"}
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "App"."Base"("Id" INT)`))
	m.ExpectQuery(`SELECT COLUMN_NAME,NULLABLE,DATA_DEFAULT`).WillReturnRows(sqlmock.NewRows([]string{"name", "nullable", "default"}))
	_, err = r.freezeMaterializedEmptyTouch(Table{Name: "Base", Columns: []Column{{Name: "Id", Writable: true}}})
	require.ErrorContains(t, err, "可写列元数据不完整")
	require.NoError(t, m.ExpectationsWereMet())
}
