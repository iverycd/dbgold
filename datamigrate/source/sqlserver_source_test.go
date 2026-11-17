package source

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleSQLServerColumnsAndNativeGeneratorsKeepSourceSemantics(t *testing.T) {
	const owner = "MiXeDOwner"
	db, mock := partitionCatalogMock(t)
	reader := WrapOracleForSQLServer(&OracleReader{db: db, owner: owner, version: 19})
	ctx := context.Background()
	cols := sqlmock.NewRows([]string{"name", "type", "length", "data_length", "precision", "scale", "nullable", "default", "char_used", "id", "owner", "virtual", "on_null"}).
		AddRow("Id", "NUMBER", 0, 22, 20, 0, "N", `"IdSequence".NEXTVAL`, "B", 1, nil, "NO", "YES").
		AddRow("Label", "VARCHAR2", 40, 160, 0, 0, "N", `'MiXeD'`, "C", 2, nil, "NO", "YES")
	mock.ExpectQuery(`SELECT c.COLUMN_NAME, c.DATA_TYPE,.*c.DEFAULT_ON_NULL`).WithArgs(owner, "OrderRows").WillReturnRows(cols)
	mock.ExpectQuery(`SELECT i.COLUMN_NAME, i.GENERATION_TYPE`).WithArgs(owner, "OrderRows").WillReturnRows(sqlmock.NewRows([]string{"name", "generation", "on_null"}).AddRow("Id", "ALWAYS", "NO"))
	info, err := reader.GetTableDDLInfo(ctx, "OrderRows")
	require.NoError(t, err)
	require.Equal(t, "ALWAYS", info.Columns[0].Identity)
	require.False(t, info.Columns[0].DefaultOnNull)
	require.True(t, info.Columns[1].DefaultOnNull)
	require.Equal(t, `'MiXeD'`, *info.Columns[1].Default)

	mock.ExpectQuery(`SELECT i.TABLE_NAME,i.COLUMN_NAME,i.GENERATION_TYPE`).WithArgs(owner).WillReturnRows(sqlmock.NewRows([]string{"table", "column", "generation", "sequence", "on_null"}).
		AddRow("OrderRows", "Id", "ALWAYS", "IdSequence", "NO"))
	mock.ExpectQuery(`SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT,DEFAULT_ON_NULL`).WithArgs(owner).WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default", "on_null"}).
		AddRow("OrderRows", "OtherId", `"SharedGen".NEXTVAL`, "NO").
		AddRow("OtherTable", "Id", `"SharedGen".NEXTVAL`, "YES").
		AddRow("UnselectedTable", "Id", `"UnselectedGen".NEXTVAL`, "NO").
		AddRow("OrderRows", "Literal", `'NEXTVAL'`, "NO"))
	mock.ExpectQuery(`SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE`).WithArgs(owner).WillReturnRows(sqlmock.NewRows([]string{"table", "name", "kind", "events", "body", "when", "owner"}).
		AddRow("OrderRows", "Audit", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW."Label" := 'NEXTVAL'; END;`, nil, owner))
	seqValues := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"}).AddRow("9007199254740993", "5", "1", "9999999999999999999999999999", "20", "N", "N")
	}
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs(owner, "IdSequence").WillReturnRows(seqValues())
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs(owner, "SharedGen").WillReturnRows(seqValues())
	mock.ExpectQuery(`SELECT SEQUENCE_NAME,TO_CHAR\(LAST_NUMBER`).WithArgs(owner).WillReturnRows(sqlmock.NewRows([]string{"name", "next", "step", "min", "max", "cache", "cycle", "ordered"}).
		AddRow("UnselectedGen", "75", "1", "1", "100", "0", "N", "N").
		AddRow("StandaloneGen", "3", "1", "1", "100", "0", "N", "N"))
	values, err := reader.SQLServerSequences(ctx, []string{"OrderRows"})
	require.NoError(t, err)
	require.Len(t, values, 3)
	require.Equal(t, "9007199254740993", values[0].StartValueText)
	require.Equal(t, owner, values[0].Native.Schema)
	require.Equal(t, "ALWAYS", values[0].Native.Bindings[0].Identity)
	require.False(t, values[0].Native.Bindings[0].OnlyWhenNull)
	require.Equal(t, "SharedGen", values[1].Native.Name)
	require.Len(t, values[1].Native.Bindings, 2)
	require.Equal(t, SequenceBinding{Schema: owner, TableName: "OtherTable", ColumnName: "Id", Kind: "default", OnlyWhenNull: true, Default: `"SharedGen".NEXTVAL`}, values[1].Native.Bindings[1])
	require.Equal(t, "StandaloneGen", values[2].Native.Name)
	require.Empty(t, values[2].Native.Bindings)
}

func TestPostgresSQLServerPagePreservesTimeTextAndEmptyBytea(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	reader := WrapPostgresForSQLServer(&PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog"}})
	metadata := sqlmock.NewRows([]string{"id", "name", "type", "kind", "type_schema", "mod", "nullable", "default", "identity", "generated", "comment", "formatted"}).
		AddRow(1, "At", "time", "b", "pg_catalog", 6, false, nil, "", "", nil, "time(6) without time zone").
		AddRow(2, "Raw", "bytea", "b", "pg_catalog", -1, true, nil, "", "", nil, "bytea")
	mock.ExpectQuery(`SELECT a.attnum,a.attname,t.typname`).WithArgs("MiXeD", "Times").WillReturnRows(metadata)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "At"::text AS "At", "Raw" FROM "MiXeD"."Times" ORDER BY ctid LIMIT 2 OFFSET 0`)).
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("At").OfType("TEXT", ""), sqlmock.NewColumn("Raw").OfType("BYTEA", []byte{})).
			AddRow("24:00:00", []byte{}).AddRow("01:02:03.123456", nil))
	cols, types, rows, err := reader.ReadPage(context.Background(), "Times", nil, 0, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"At", "Raw"}, cols)
	require.Equal(t, []string{"TIME", "BYTEA"}, types)
	require.Equal(t, [][]any{{"24:00:00", []byte{}}, {"01:02:03.123456", nil}}, rows)
}

func TestSQLServerDecimalProfileReadsOnlyRequestedColumnsExactly(t *testing.T) {
	const exact = "12345678901234567890.123456789012345678"
	for _, engine := range []string{"oracle", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			var reader interface {
				SQLServerDecimalProfile(context.Context, string, []string, func(string, interface{}) error) error
			}
			query := `SELECT "Verbatim"::text FROM "MiXeD"."Exact"`
			reader = WrapPostgresForSQLServer(&PostgresReader{db: db, schema: "MiXeD"})
			if engine == "oracle" {
				query = `SELECT TO_CHAR("Verbatim", 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''') FROM "MiXeD"."Exact"`
				reader = WrapOracleForSQLServer(&OracleReader{db: db, owner: "MiXeD", version: 11})
			}
			mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$").WillReturnRows(sqlmock.NewRows([]string{"Verbatim"}).AddRow([]byte(exact)).AddRow(nil)).RowsWillBeClosed()
			var cells []any
			err := reader.SQLServerDecimalProfile(context.Background(), "Exact", []string{"Verbatim"}, func(name string, value interface{}) error {
				require.Equal(t, "Verbatim", name)
				cells = append(cells, value)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, []any{exact}, cells)
		})
	}
}

func TestPostgresSQLServerSequenceRetainsExternalConsumersAndExactKind(t *testing.T) {
	pg := PGSequence{
		Name: "MiXeDSeq", Schema: "App", Increment: 5, Min: 1, Max: 999, Cache: 4,
		SequenceInfo: SequenceInfo{StartValue: 21},
		Bindings: []PGSequenceBinding{
			{Schema: "External", TableName: "Peer", ColumnName: "Id", Default: `nextval('"App"."MiXeDSeq"'::regclass)`},
			{Schema: "App", TableName: "One", ColumnName: "Id", Default: `nextval('"App"."MiXeDSeq"'::regclass)`, Owned: true},
			{Schema: "App", TableName: "Two", ColumnName: "Id", Default: `nextval('"App"."MiXeDSeq"'::regclass) + 1`},
		},
	}
	seq := pgSQLServerSequence(pg, "App", map[string]bool{"One": true})
	require.Equal(t, "One", seq.TableName)
	require.Equal(t, "21", seq.StartValueText)
	require.Equal(t, "5", seq.Native.IncrementBy)
	require.Equal(t, "External", seq.Native.Bindings[0].Schema)
	require.Equal(t, "default", seq.Native.Bindings[0].Kind)
	require.Equal(t, "serial", seq.Native.Bindings[1].Kind)
	require.Equal(t, "expression", seq.Native.Bindings[2].Kind)
}

func TestOracleSQLServerReportsChecksWithoutDuplicateNotNull(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	reader := WrapOracleForSQLServer(&OracleReader{db: db, owner: "MiXeD", version: 11})
	reader.metadata["Orders"] = &TableDDLInfo{TableName: "Orders", Columns: []ColumnInfo{
		{Name: "Id", IsNullable: false}, {Name: "Maybe", IsNullable: true},
	}}
	mock.ExpectQuery("SELECT TABLE_NAME,CONSTRAINT_NAME,SEARCH_CONDITION").WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "constraint", "condition"}).
		AddRow("Orders", "SysNotNull", `("Id" IS NOT NULL)`).
		AddRow("Orders", "RequireMaybe", `"Maybe" IS NOT NULL`).
		AddRow("Orders", "PositiveOnly", `"Id" > 0`))
	mock.ExpectQuery("SELECT TABLE_NAME,CONSTRAINT_NAME,INDEX_NAME,INDEX_OWNER").WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "constraint", "index", "idxowner", "status", "validated", "deferrable", "deferred"}).
		AddRow("Orders", "DeferredUnique", "UniqueIdx", "MiXeD", "ENABLED", "VALIDATED", "DEFERRABLE", "DEFERRED"))
	constraints, err := reader.SQLServerUnsupportedConstraints(context.Background())
	require.NoError(t, err)
	require.Len(t, constraints, 3)
	require.Equal(t, "RequireMaybe", constraints[0].Name)
	require.Equal(t, `"Id" > 0`, constraints[1].Definition)
	require.Equal(t, "DeferredUnique", constraints[2].Name)
	require.Contains(t, constraints[2].Unsupported, "DEFERRABLE")
}

func TestPostgresSQLServerViewsKeepDefinitionAndColumns(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	db.SetMaxOpenConns(1)
	reader := WrapPostgresForSQLServer(&PostgresReader{db: db, schema: "MiXeD", profile: PGProfile{Catalog: "pg_catalog"}})
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET LOCAL search_path TO "pg_catalog", pg_temp`)).WillReturnResult(sqlmock.NewResult(0, 0))
	const body = `SELECT "MiXeD"."GreatThing"."Title", 'KeepCase!' AS "Shown" FROM "MiXeD"."GreatThing"`
	mock.ExpectQuery(`SELECT c.relname,pg_get_viewdef`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"name", "definition", "kind"}).
		AddRow("BadView", nil, "v").AddRow("GoodView", body+";", "v").AddRow("Materialized", "SELECT 1", "m"))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT a.attnum,a.attname,t.typname`).WithArgs("MiXeD", "GoodView").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "kind", "type_schema", "mod", "nullable", "default", "identity", "generated", "comment", "formatted"}).
		AddRow(1, "Title", "text", "b", "pg_catalog", -1, true, nil, "", "", nil, "text").
		AddRow(2, "Shown", "text", "b", "pg_catalog", -1, true, nil, "", "", nil, "text"))
	views, err := reader.GetViews(context.Background())
	require.NoError(t, err)
	require.Len(t, views, 3)
	require.NotEmpty(t, views[0].Unsupported)
	require.Equal(t, "GoodView", views[1].ViewName)
	require.Equal(t, body, views[1].Definition)
	require.Equal(t, "Shown", views[1].Columns[1].Name)
	require.NotEmpty(t, views[2].Unsupported)
}
