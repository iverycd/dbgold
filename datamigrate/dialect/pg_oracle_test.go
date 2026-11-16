package dialect

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestPGOracleDefaults(t *testing.T) {
	for _, tc := range []struct{ raw, dt, target, want string }{
		{"'a''B  '::character varying", "varchar", "VARCHAR2(20 CHAR)", " DEFAULT 'a''B  '"},
		{"true", "bool", "NUMBER(1,0)", " DEFAULT 1"},
		{"'12'::integer", "int4", "NUMBER(10,0)", " DEFAULT 12"},
		{"'1e-130'::numeric", "numeric", "NUMBER", " DEFAULT 0." + strings.Repeat("0", 129) + "1"},
		{"'2024-01-02'::date", "date", "DATE", " DEFAULT DATE '2024-01-02'"},
		{"'2024-01-02 03:00:00+03'::timestamp with time zone", "timestamptz", "TIMESTAMP(6) WITH TIME ZONE", " DEFAULT TO_TIMESTAMP_TZ('2024-01-02 00:00:00.000000 +00:00', 'YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')"},
		{"''::text", "text", "CLOB", " DEFAULT EMPTY_CLOB()"},
		{"''::varchar", "varchar", "VARCHAR2(20 CHAR)", " DEFAULT NULL"},
		{"'192.0.2.0/24'::inet", "inet", "VARCHAR2(49 CHAR)", " DEFAULT '192.0.2.0/24'"},
		{"nextval('public.s'::regclass)", "int8", "NUMBER(19,0)", ""},
		{"current_date", "date", "DATE", " DEFAULT TRUNC(CURRENT_DATE)"},
		{"now()", "timestamp", "TIMESTAMP(6)", " DEFAULT CAST(CURRENT_TIMESTAMP(6) AS TIMESTAMP(6))"},
		{"localtimestamp(3)", "timestamp", "TIMESTAMP(6)", " DEFAULT LOCALTIMESTAMP(3)"},
	} {
		c := source.ColumnInfo{DataType: tc.dt, Default: &tc.raw, IsNullable: true, DateTimePrecision: 6}
		got, err := PGOracleDefault(c, tc.target)
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.want, got)
	}
	for _, raw := range []string{"nextval('s'::regclass)+1", "other.now()", "'abcd'::varchar(2)", "'1e126'::numeric", "1 + 2"} {
		c := source.ColumnInfo{DataType: "numeric", Default: &raw}
		_, err := PGOracleDefault(c, "NUMBER")
		require.Error(t, err, raw)
	}
	raw := "now()"
	require.NotEmpty(t, PGOracleDefaultWarnings(source.ColumnInfo{DataType: "timestamp", DateTimePrecision: 6, Default: &raw}))
	raw = "'now()'::text"
	require.Empty(t, PGOracleDefaultWarnings(source.ColumnInfo{DataType: "text", Default: &raw}))
	for _, raw := range []string{`sys_catalog.nextval('s'::regclass)`, `("sys_catalog"."nextval"('s'::"sys_catalog"."regclass"))`} {
		require.True(t, PGOracleDirectSequenceDefault(raw))
		got, err := PGOracleDefault(source.ColumnInfo{DataType: "int8", Default: &raw}, "NUMBER(19,0)")
		require.NoError(t, err)
		require.Empty(t, got)
	}
	require.False(t, PGOracleDirectSequenceDefault(`custom.nextval('s'::regclass)`))
	raw = "'😀x'::varchar"
	clause, err := PGOracleDefault(source.ColumnInfo{DataType: "varchar", Length: 2, Default: &raw}, "NVARCHAR2(4)")
	require.NoError(t, err)
	require.Equal(t, ` DEFAULT UNISTR('\D83D\DE00x')`, clause)
}

func TestPGOracleTableNamesRangesAndOldMySQL(t *testing.T) {
	d := NewOracle()
	info := &source.TableDDLInfo{TableName: "Mixed", Columns: []source.ColumnInfo{{Name: "id", DataType: "int8"}, {Name: "ok", DataType: "bool", IsNullable: true}}}
	ss, err := d.CreateTableStatements("APP", info, "kingbase", TypeOpt{}, strings.ToUpper)
	require.NoError(t, err)
	sql := JoinSQL(ss)
	require.Contains(t, sql, `"APP"."MIXED"`)
	require.Contains(t, sql, "BETWEEN -9223372036854775808 AND 9223372036854775807")
	require.Contains(t, sql, `CHECK ("OK" IN (0, 1))`)
	info.Columns = append(info.Columns, source.ColumnInfo{Name: "ID", DataType: "int4"})
	_, err = d.CreateTableStatements("APP", info, "postgres", TypeOpt{}, strings.ToUpper)
	require.ErrorContains(t, err, "冲突")
	info.Columns = []source.ColumnInfo{{Name: "id", DataType: "bigint"}}
	ss, err = d.CreateTableStatements("APP", info, "mysql", TypeOpt{}, strings.ToUpper)
	require.NoError(t, err)
	require.Contains(t, JoinSQL(ss), "NUMBER(20,0)")
	require.NotContains(t, JoinSQL(ss), "BETWEEN")
	info.Columns = []source.ColumnInfo{{Name: "Title", DataType: "varchar", Length: 2}}
	ss, err = d.CreateTableStatements("APP", info, "postgres", TypeOpt{UseNvarchar2: true}, strings.ToUpper)
	require.NoError(t, err)
	require.Contains(t, JoinSQL(ss), `"TITLE" NVARCHAR2(4)`)
	require.Contains(t, JoinSQL(ss), `CONSTRAINT "CK_MIXED_TITLE_PGLEN" CHECK (LENGTH4("TITLE") <= 2)`)
	require.LessOrEqual(t, len(PGOracleLengthCheckName(strings.Repeat("T", 30), strings.Repeat("C", 30))), 30)
}
