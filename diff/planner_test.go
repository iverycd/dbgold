package diff

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"

	"github.com/stretchr/testify/require"
)

func TestSchemaCompareNormalizesMappedTypesAndRenamedIndex(t *testing.T) {
	memo := testColumn("Memo", "varchar", "varchar(40)", true)
	memo.Length = 40
	count := testColumn("Count", "int", "int(11)", false)
	left := testCatalog("mysql", "app", &Table{Name: "Orders", Columns: []Column{memo, count},
		Indexes: []Key{{Name: "Idx", Kind: "index", Method: "BTREE", Columns: []string{"Memo"}}}})
	right := testCatalog("postgres", "target", &Table{Name: "orders",
		Columns: []Column{testColumn("memo", "varchar", "character varying(40)", true), testColumn("count", "int4", "integer", false)},
		Indexes: []Key{{Name: "orders_idx", Kind: "index", Method: "btree", Columns: []string{"memo"}, Descending: []bool{false}}}})
	require.Empty(t, Build(left, right, Options{}))

	right.Tables["orders"].Indexes = []Key{{Name: "idx", Kind: "index", Method: "btree", Columns: []string{"count"}}}
	changed := Build(left, right, Options{})
	require.Len(t, changed, 1)
	require.Equal(t, "index", changed[0].Kind)
	require.Equal(t, Manual, changed[0].Status)
	require.NotNil(t, changed[0].Source)
	require.NotNil(t, changed[0].Expected)
	require.NotNil(t, changed[0].Target)
}

func TestSchemaCompareFoldedCollisionIncludesUnselectedTables(t *testing.T) {
	left := testCatalog("mysql", "app", &Table{Name: "Order", Columns: []Column{testColumn("id", "int", "int", false)}},
		&Table{Name: "order", Columns: []Column{testColumn("id", "int", "int", false)}})
	right := testCatalog("postgres", "target")
	result := Build(left, right, Options{Tables: []string{"Order"}})
	require.Len(t, result, 1)
	require.Equal(t, Manual, result[0].Status)
	require.Contains(t, result[0].Reason, "多个源表")
	require.Empty(t, result[0].SQL)

	result = Build(left, right, Options{Tables: []string{"Order"}, PreserveCase: true})
	require.Len(t, result, 1)
	require.Equal(t, Applicable, result[0].Status)
	require.Contains(t, result[0].SQL[0], `"target"."Order"`)
}

func TestSchemaCompareReportsAllTargetOnlyTables(t *testing.T) {
	left := testCatalog("postgres", "source")
	right := testCatalog("postgres", "target", &Table{Name: "a"}, &Table{Name: "b"}, &Table{Name: "c"})
	all := Build(left, right, Options{})
	require.Len(t, all, 3)
	for _, change := range all {
		require.Equal(t, Manual, change.Status)
		require.Equal(t, "table_extra", change.Kind)
	}
	selected := Build(left, right, Options{Tables: []string{"b", "missing"}})
	require.Len(t, selected, 2)
	require.Equal(t, "b", selected[0].Table)
	require.Equal(t, "table_extra", selected[0].Kind)
	require.Equal(t, "table_unsupported", selected[1].Kind)
}

func TestSchemaCompareMySQLCreateUsesStoredTableSpelling(t *testing.T) {
	left := testCatalog("mysql", "source", &Table{Name: "Orders",
		Columns: []Column{testColumn("ID", "int", "int", true)},
		Engine:  "InnoDB", Charset: "utf8mb4", Collation: "utf8mb4_general_ci"})
	right := testCatalog("mysql", "target")
	right.CaseMode = 1
	result := Build(left, right, Options{Tables: []string{"Orders"}})
	require.Len(t, result, 1)
	require.Equal(t, "orders", result[0].Table)
	require.Contains(t, result[0].SQL[0], "`target`.`orders`")
	require.Contains(t, result[0].SQL[0], "`ID`")
}

func TestSchemaCompareAdditiveDependenciesAndManualColumnChanges(t *testing.T) {
	c := testColumn("code", "varchar", "character varying(10)", true)
	def := "'x'::character varying"
	left := testCatalog("postgres", "app", &Table{Name: "orders",
		Columns: []Column{testColumn("id", "int4", "integer", false), c, testColumn("required", "int4", "integer", false)},
		Keys:    []Key{{Name: "orders_pkey", Kind: "primary", Columns: []string{"id"}}},
		Indexes: []Key{{Name: "code_idx", Kind: "index", Columns: []string{"code"}}}})
	existing := testColumn("id", "int4", "integer", true)
	existing.Default = &def
	right := testCatalog("postgres", "target", &Table{Name: "orders",
		Columns: []Column{existing, testColumn("target_only", "int4", "integer", true)}})
	var add, index Change
	for _, change := range Build(left, right, Options{}) {
		if change.Kind == "column_missing" && change.Object == "code" {
			add = change
		} else if change.Kind == "index" {
			index = change
		} else {
			require.Equal(t, Manual, change.Status, "%+v", change)
			require.Empty(t, change.SQL)
		}
	}
	require.Equal(t, Applicable, add.Status)
	require.Equal(t, Applicable, index.Status)
	require.Equal(t, []string{add.ID}, index.DependsOn)
	require.Less(t, add.Order, index.Order)
}

func TestSchemaCompareDamengMapsReservedColumns(t *testing.T) {
	left := testCatalog("mysql", "app", &Table{Name: "Orders", Columns: []Column{
		testColumn("RowID", "int", "int", false), testColumn("Row_ID", "int", "int", true),
	}, Keys: []Key{{Name: "PRIMARY", Kind: "primary", Columns: []string{"RowID"}}}})
	right := testCatalog("dameng", "App Owner")
	result := Build(left, right, Options{})
	require.Len(t, result, 1)
	require.Equal(t, Applicable, result[0].Status)
	require.Contains(t, result[0].SQL[0], `CREATE TABLE "App Owner"."ORDERS"`)
	require.Contains(t, result[0].SQL[0], `PRIMARY KEY ("ROW_ID_2")`)
	require.NotContains(t, result[0].SQL[0], "DROP")
}

func TestSchemaCompareKeepsNativeMySQLSignednessAndLiterals(t *testing.T) {
	id := testColumn("id", "int", "int(11) unsigned", false)
	text := testColumn("text", "varchar", "varchar(20)", true)
	text.Charset, text.Collation = "utf8mb4", "utf8mb4_general_ci"
	text.Length = 20
	value := "NULL"
	text.Default = &value
	left := testCatalog("mysql", "src", &Table{Name: "a", Columns: []Column{id, text}, Engine: "InnoDB", Charset: text.Charset, Collation: text.Collation})
	result := Build(left, testCatalog("mysql", "dst"), Options{})
	require.Len(t, result, 1)
	require.Equal(t, Applicable, result[0].Status)
	require.Contains(t, result[0].SQL[0], "int(11) unsigned NOT NULL")
	require.Contains(t, result[0].SQL[0], "DEFAULT 'NULL'")

	result = Build(left, testCatalog("postgres", "dst"), Options{})
	require.Len(t, result, 1)
	require.Equal(t, Manual, result[0].Status)
	require.Contains(t, result[0].Reason, "无符号")
}

func TestSchemaCompareCommentsQuoteIdentifiersAndKeepExistingDefault(t *testing.T) {
	src := testColumn("a\"b", "varchar", "character varying(20)", true)
	dst := src
	want, old := "'Some Value'::character varying", "'another'::character varying"
	src.Default, dst.Default = &want, &old
	src.Comment = "中文 'text' " + string(rune(92)) + "path"
	left := testCatalog("postgres", "src", &Table{Name: "table", Columns: []Column{src}})
	right := testCatalog("postgres", "dst", &Table{Name: "table", Columns: []Column{dst}})
	result := Build(left, right, Options{})
	require.Len(t, result, 2)
	for _, change := range result {
		switch change.Kind {
		case "column_definition":
			require.Equal(t, Manual, change.Status)
		case "column_comment":
			require.Equal(t, Applicable, change.Status)
			require.Contains(t, change.SQL[0], `"dst"."table"."a""b" IS '中文 ''text'' `)
			require.True(t, strings.HasSuffix(change.SQL[0], string(rune(92))+"path'"))
		default:
			t.Fatalf("unexpected change: %+v", change)
		}
	}
}

func testColumn(name, typ, formatted string, nullable bool) Column {
	return Column{ColumnInfo: source.ColumnInfo{Name: name, DataType: typ, FormattedType: formatted, IsNullable: nullable}}
}

func testCatalog(product, namespace string, tables ...*Table) Catalog {
	out := Catalog{Product: product, Namespace: namespace, Tables: map[string]*Table{}, Occupied: map[string]string{}, IdentifierLimit: 63}
	for _, table := range tables {
		out.Tables[table.Name] = table
	}
	return out
}
