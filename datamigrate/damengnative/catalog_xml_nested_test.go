package damengnative

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMaterializedPartitionBufferRestoresOnlyProvenXMLAttributes(t *testing.T) {
	ddl := `CREATE TABLE "App"."Storage"("Id" INT,"Document" TEXT,"Plain" TEXT DEFAULT '<TEXT/>') STORAGE(ON "MAIN",CLUSTERBTR)`
	columns := []Column{{Name: "Id", DataType: "INT", SQLType: "INT"}, {Name: "Document", DataType: "TEXT", SQLType: "XMLTYPE", Scale: 8}, {Name: "Plain", DataType: "TEXT", SQLType: "TEXT"}}
	got, err := MaterializedPartitionBufferDDL(ddl, "App", "Buffer", "P", columns)
	require.NoError(t, err)
	require.Contains(t, got, `"Document" XMLTYPE`)
	require.Contains(t, got, `"Plain" TEXT DEFAULT '<TEXT/>'`)
	require.Contains(t, got, `PARTITION BY RANGE("Id")`)
}

func TestMaterializedEmptyEpochRestoresXMLAndNeverTouchesProtectedPrebuiltStorage(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	m.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL`).WithArgs("TABLE", "Base", "App").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TABLE "App"."Base"("Id" INT,"Document" TEXT)`))
	m.ExpectQuery(`SELECT COLUMN_NAME,NULLABLE,DATA_DEFAULT`).WithArgs("App", "Base").WillReturnRows(sqlmock.NewRows([]string{"column", "nullable", "default"}).AddRow("Id", "Y", nil).AddRow("Document", "Y", nil))
	m.ExpectQuery(`SELECT CONSTRAINT_NAME,TABLE_NAME,CONSTRAINT_TYPE,GENERATED,STATUS,VALIDATED`).WillReturnRows(sqlmock.NewRows([]string{"name", "table", "type", "generated", "status", "validated"}))
	base := Table{Name: "Base", Rows: 0, Columns: []Column{{Name: "Id", DataType: "INT", SQLType: "INT", Writable: true}, {Name: "Document", DataType: "TEXT", SQLType: "XMLTYPE", Scale: 8, Writable: true}}}
	r.snapshot.Objects = []Object{{ID: "base", Kind: "table", Name: "Base"}, {ID: "store", Kind: "table", Name: "Store"}, {ID: "helper", Kind: "table", Name: "AHelper", Generated: true}, {ID: "mv", Kind: "materialized_view", Name: "Mv", Table: "Store", Dependencies: []string{"base", "store", "helper"}, Materialized: &MaterializedState{History: &MaterializedHistoryPlan{Views: []MaterializedHistoryView{{ObjectID: "mv", Name: "Mv", Table: "Store", NativeStaleness: "NEEDS_COMPILE"}}}}}}
	r.snapshot.Tables = []Table{base, {Name: "Store", Rows: 2, Columns: base.Columns}, {Name: "AHelper", Rows: 5, Columns: base.Columns}}
	r.materializedHistoryTables()
	view := r.snapshot.Objects[3].Materialized.History.Views[0]
	require.Empty(t, view.TouchSQL, "受保护原快照和已生成辅助表都不能当业务基表UPDATE")
	p := view.EmptyTouch
	require.NotNil(t, p)
	require.Equal(t, "Base", p.Table)
	require.Contains(t, p.DefinitionSQL, `"Document" XMLTYPE`)
	require.NoError(t, m.ExpectationsWereMet())
}
