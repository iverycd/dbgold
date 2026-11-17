package source

import (
	"context"
	"database/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCrossNextExactAndDescending(t *testing.T) {
	n, e := crossNext("9223372036854775808", "5", sql.NullString{})
	require.NoError(t, e)
	require.Equal(t, "9223372036854775808", n)
	n, e = crossNext("1", "-5", sql.NullString{String: "-9223372036854775808", Valid: true})
	require.NoError(t, e)
	require.Equal(t, "-9223372036854775813", n)
	_, e = crossNext("1", "0", sql.NullString{})
	require.Error(t, e)
}
func TestCrossReadPageKeepsNamesExactDecimalsAndNUL(t *testing.T) {
	for _, product := range []string{"dameng", "sqlserver"} {
		t.Run(product, func(t *testing.T) {
			db, m, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			db.SetMaxOpenConns(1)
			info := &TableDDLInfo{TableName: "MiXeD", Columns: []ColumnInfo{{Name: "ID", DataType: "decimal", Precision: 38}, {Name: "Text", DataType: "nvarchar"}}}
			m.ExpectQuery("SELECT .* AS .* FROM .*MiXeD.* ORDER BY .* OFFSET").WithArgs(int64(0), int64(10)).WillReturnRows(sqlmock.NewRows([]string{"ID", "Text"}).AddRow("12345678901234567890123456789012345678", "中\x00文"))
			cols, types, rows, e := crossReadPage(context.Background(), db, info, []string{"ID"}, 0, 10, product, "App")
			require.NoError(t, e)
			require.Equal(t, []string{"ID", "Text"}, cols)
			require.Equal(t, []string{"DECIMAL", "NVARCHAR"}, types)
			require.Equal(t, "中\x00文", rows[0][1])
			require.Equal(t, "12345678901234567890123456789012345678", rows[0][0])
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
func TestSQLServerOracleMetadataUsesDBOAndExactIdentity(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &SQLServerOracleReader{&SQLServerDamengReader{&SQLServerReader{db: db}}}
	m.ExpectQuery("SELECT c.name,ty.name,.*").WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "nullable", "default", "computed", "user_defined", "seed", "step"}).AddRow("BigID", "decimal", 17, 28, 0, false, nil, false, false, "9223372036854775808", "5"))
	info, e := r.GetTableDDLInfo(context.Background(), "MiXeD")
	require.NoError(t, e)
	require.Equal(t, "MiXeD", info.TableName)
	require.Equal(t, "BigID", info.Columns[0].Name)
	require.Equal(t, "9223372036854775808", info.Columns[0].SQLServerIdentitySeed)
	require.Equal(t, "dbo", r.SourceSchema())
	require.NoError(t, m.ExpectationsWereMet())
}
func TestDamengCrossViewsAreRaw(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &DaMengCrossReader{&DaMengReader{db: db, schema: "MiXeD"}}
	definition := `SELECT "Id", 'KeepCase' FROM "MiXeD"."Orders"`
	m.ExpectQuery("SELECT VIEW_NAME,TEXT FROM ALL_VIEWS WHERE OWNER=\\?").WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"name", "text"}).AddRow("ViewName", definition))
	m.ExpectQuery("SELECT c.TABLE_NAME,c.COLUMN_NAME").WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "type", "length", "precision", "scale", "nullable"}).AddRow("ViewName", "Id", "INT", 0, 10, 0, "N"))
	v, e := r.GetViews(context.Background())
	require.NoError(t, e)
	require.Equal(t, "ViewName", v[0].ViewName)
	require.Equal(t, definition, v[0].Definition)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestCrossSequenceDefaultsAreOnlyDirectBindings(t *testing.T) {
	for _, tc := range []struct {
		product, raw string
		valid        bool
	}{
		{"sqlserver", "((NEXT VALUE FOR [dbo].[SharedSeq]))", true}, {"sqlserver", "NEXT VALUE FOR [other].[SharedSeq]", false}, {"sqlserver", "NEXT VALUE FOR [dbo].[SharedSeq]+1", false}, {"sqlserver", "NEXT VALUE FOR [dbo].[SharedSeq])+(1", false},
		{"dameng", `"Src"."SharedSeq".NEXTVAL`, true}, {"dameng", `"Src"."SharedSeq".NEXTVAL+1`, false}, {"dameng", `'SharedSeq.NEXTVAL'`, false},
	} {
		schema := "dbo"
		if tc.product == "dameng" {
			schema = "Src"
		}
		require.Equal(t, tc.valid, directCrossSequenceDefault(tc.raw, tc.product, schema, "SharedSeq"), tc.raw)
	}
}
func TestCrossIdentityPreservesDecimalBoundsAndSeed(t *testing.T) {
	s := crossIdentity("MiXeD", "Id", "dbo", "9223372036854775808", "-3", sql.NullString{}, ColumnInfo{DataType: "numeric", Precision: 28}, "sqlserver")
	require.Empty(t, s.Unsupported)
	require.Equal(t, "9999999999999999999999999999", s.Native.MaxValue)
	require.Equal(t, "9223372036854775808", s.Native.SeedValue)
	require.Equal(t, "-3", s.Native.IncrementBy)
}
func TestSQLServerCrossSequenceInventoryKeepsSharedAndCrossSchemaBindings(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	db.SetMaxOpenConns(1)
	r := &SQLServerOracleReader{&SQLServerDamengReader{&SQLServerReader{db: db}}}
	m.ExpectQuery("SELECT t.name,c.name.*t.schema_id=SCHEMA_ID\\('dbo'\\)").WillReturnRows(sqlmock.NewRows([]string{"table", "col", "seed", "step", "last", "type", "precision"}).AddRow("MiXeD", "Id", "9223372036854775808", "5", nil, "numeric", 28))
	m.ExpectQuery("SELECT q.name.*q.schema_id=SCHEMA_ID\\('dbo'\\)").WillReturnRows(sqlmock.NewRows([]string{"name", "seed", "step", "current", "min", "max", "cycle", "exhausted", "cache"}).AddRow("SharedSeq", "1", "3", "7", "-100", "100000", false, false, "0"))
	m.ExpectQuery("SELECT s.name,SCHEMA_NAME.*").WillReturnRows(sqlmock.NewRows([]string{"seq", "schema", "table", "col", "default"}).AddRow("SharedSeq", "dbo", "MiXeD", "Id2", "NEXT VALUE FOR [dbo].[SharedSeq]").AddRow("SharedSeq", "other", "MiXeD", "Id", "NEXT VALUE FOR [dbo].[SharedSeq]"))
	seqs, e := r.GetSequences(context.Background())
	require.NoError(t, e)
	require.Len(t, seqs, 2)
	require.Equal(t, "MiXeD", seqs[0].TableName)
	require.Equal(t, "9223372036854775808", seqs[0].StartValueText)
	require.Len(t, seqs[1].Native.Bindings, 2)
	require.Equal(t, "other", seqs[1].Native.Bindings[1].Schema)
	require.NotEmpty(t, seqs[1].Unsupported)
	require.NoError(t, m.ExpectationsWereMet())
}
