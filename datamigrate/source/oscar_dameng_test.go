package source

import (
	"context"
	"io"
	"math"
	"testing"

	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

type oscarDamengReaderFixture struct {
	oscarReaderFixture
	textColumns []int
}

func (f *oscarDamengReaderFixture) OpenOscarDamengCursor(ctx context.Context, session uint64, query string, columns []int) (uint64, error) {
	f.textColumns = append([]int(nil), columns...)
	return f.OpenCursor(ctx, session, query)
}

func TestOscarDamengStreamUsesNativeTextAndKeepsExactSourceNames(t *testing.T) {
	const table = `Table"Name`
	f := &oscarDamengReaderFixture{oscarReaderFixture: oscarReaderFixture{
		cols: []jdbcbridge.MigrationColumn{
			{Name: "Bits", Type: "BIT VARYING", SQLType: -7, Size: 10, Ordinal: 1},
			{Name: "Bool", Type: "BOOLEAN", SQLType: 16, Ordinal: 2},
			{Name: "aRRaY", Type: "_int4", SQLType: 2003, Ordinal: 3},
			{Name: `Xml"Name`, Type: "XMLTYPE", SQLType: 2009, Ordinal: 4},
			{Name: "Tz", Type: "timestamptz", SQLType: 93, Ordinal: 5},
			{Name: "Interval", Type: "INTERVAL DAY(3)TO SECOND(9)", Ordinal: 6},
			{Name: "Exact", Type: "DECIMAL", SQLType: 3, Ordinal: 7},
			{Name: "Always", Type: "BLOB", SQLType: 2004, Ordinal: 8},
		},
		fetches: []oscarFixtureFetch{
			{rows: [][]any{{"001", false, `[0:1]={NULL,2}`, "<X> y </X>", "0001-01-02 03:04:05.123456789+08:30", "-001 01:02:03.123456789", "12345678901234567890.00", []byte{0, 1}}}},
			{rows: [][]any{{"", true, "{}", nil, nil, nil, "0.01", []byte{}}}, done: true},
		},
	}}
	raw := &OscarReader{bridge: f, session: 1, schema: "MiXeD"}
	reader := WrapOscarForDameng(raw).(*OscarDamengReader)
	require.Same(t, reader, WrapOscarForDameng(reader))
	wantTypes := []string{"VARBIT", "BOOL", "ARRAY", "XMLTYPE", "TIMESTAMP WITH TIME ZONE", "INTERVAL DAY TO SECOND", "NUMERIC", "BLOB"}
	info, err := reader.GetTableDDLInfo(t.Context(), table)
	require.NoError(t, err)
	require.Equal(t, table, info.TableName)
	for i, col := range info.Columns {
		require.Equal(t, f.cols[i].Name, col.Name)
		require.Equal(t, wantTypes[i], col.DataType)
	}

	var rows [][]any
	err = reader.StreamOscarDamengTable(t.Context(), table, 5, func(names, types []string, batch [][]any) error {
		require.Equal(t, []string{"Bits", "Bool", "aRRaY", `Xml"Name`, "Tz", "Interval", "Exact", "Always"}, names)
		require.Equal(t, wantTypes, types)
		rows = append(rows, batch...)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, `SELECT CAST("Bits" AS TEXT),"Bool",CAST("aRRaY" AS TEXT),"Xml""Name","Tz","Interval","Exact","Always" FROM "MiXeD"."Table""Name"`, f.query)
	require.Equal(t, []int{0, 2, 3, 4, 5}, f.textColumns)
	require.Len(t, rows, 2)
	require.Equal(t, "001", rows[0][0])
	require.Equal(t, "", rows[1][0])
	require.Equal(t, "0001-01-02 03:04:05.123456789+08:30", rows[0][4])
	require.Equal(t, "12345678901234567890.00", rows[0][6])
	require.Equal(t, 1, f.closed)
}

func TestOscarDamengFixedBitRejectsDriverBooleanOrMissingLeadingZero(t *testing.T) {
	for _, value := range []any{true, "1", "01x"} {
		f := &oscarDamengReaderFixture{oscarReaderFixture: oscarReaderFixture{
			cols:    []jdbcbridge.MigrationColumn{{Name: "Bit", Type: "BIT", Size: 3, Ordinal: 1}},
			fetches: []oscarFixtureFetch{{rows: [][]any{{value}}, done: true}},
		}}
		r := WrapOscarForDameng(&OscarReader{bridge: f, session: 1, schema: "MiXeD"}).(*OscarDamengReader)
		err := r.StreamOscarDamengTable(t.Context(), `Table"Name`, 5, func(_, _ []string, _ [][]any) error {
			t.Fatal("invalid source row reached writer")
			return nil
		})
		require.ErrorContains(t, err, "列 Bit")
		require.Equal(t, 1, f.closed)
	}
}

func TestOscarDamengUnboundedVarcharUsesTextBinding(t *testing.T) {
	p := newOscarDamengProjection([]jdbcbridge.MigrationColumn{
		{Name: "Unbounded", Type: "VARCHAR", Size: -1},
		{Name: "Bounded", Type: "NVARCHAR2", Size: 40},
	})
	require.Equal(t, []string{"TEXT", "NVARCHAR2"}, p.types)
	require.Equal(t, []string{`"Unbounded"`, `"Bounded"`}, p.expressions)
}

func TestOscarDamengContentValidationUsesTargetBitEncoding(t *testing.T) {
	const table = `Table"Name`
	const pkQuery = `SELECT TABLE_NAME,PK_NAME,COLUMN_NAME,KEY_SEQ FROM V_SYS_PRIMARY_KEYS WHERE TABLE_SCHEM=? AND TABLE_NAME=? ORDER BY TABLE_NAME,PK_NAME,KEY_SEQ`
	f := &oscarDamengReaderFixture{oscarReaderFixture: oscarReaderFixture{
		cols: []jdbcbridge.MigrationColumn{
			{Name: "Flag", Type: "BOOL", Ordinal: 1},
			{Name: "Bit", Type: "BIT", Size: 3, Ordinal: 2},
			{Name: "Array", Type: "integer[]", Ordinal: 3},
			{Name: "Calendar", Type: "INTERVALYTM", Ordinal: 4},
		},
		queries: map[string][][]any{pkQuery: {}},
		fetches: []oscarFixtureFetch{{rows: [][]any{{"001", true, "[0:1]={NULL,2}"}}, done: true}},
	}}
	r := WrapOscarForDameng(&OscarReader{bridge: f, session: 1, schema: "MiXeD"}).(*OscarDamengReader)
	metadata, err := r.DescribeValidationTable(t.Context(), table)
	require.NoError(t, err)
	require.Equal(t, validation.Bool, metadata.Columns[0].Kind)
	require.Equal(t, validation.Binary, metadata.Columns[1].Kind)
	require.Equal(t, validation.Text, metadata.Columns[2].Kind)
	require.Equal(t, validation.Unsupported, metadata.Columns[3].Kind)
	_, err = r.OpenValidationScan(t.Context(), table, metadata.Columns)
	require.ErrorContains(t, err, "INTERVAL YEAR TO MONTH")

	scan, err := r.OpenValidationScan(t.Context(), table, []validation.Column{metadata.Columns[1], metadata.Columns[0], metadata.Columns[2]})
	require.NoError(t, err)
	defer scan.Close()
	row, err := scan.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, []any{[]byte{0, 0, 1}, true, "[0:1]={NULL,2}"}, row)
	_, err = scan.Next(t.Context())
	require.ErrorIs(t, err, io.EOF)
}

func TestOscarDamengSerialSeedUsesReadOnlyStateAndExistingMaximum(t *testing.T) {
	def := `nextval('"MiXeD"."SequenceName"'::regclass)`
	const table = `Table"Name`
	f := &oscarReaderFixture{
		cols: []jdbcbridge.MigrationColumn{{Name: "SerialId", Type: "INT4", AutoIncrement: "YES", Default: &def, Ordinal: 1}},
		queries: map[string][][]any{
			`SELECT c.RELKIND FROM SYS_CLASS c JOIN SYS_NAMESPACE n ON n.OID=c.RELNAMESPACE
WHERE n.NSPNAME=? AND c.RELNAME=?`: {{"S"}},
			`SELECT START_VALUE,INCREMENT_BY,MAX_VALUE,IS_CYCLED,LAST_VALUE,IS_CALLED FROM "MiXeD"."SequenceName"`: {{int64(1), int64(3), int64(math.MaxInt32), false, int64(10), true}},
			`SELECT MAX("SerialId") FROM "MiXeD"."Table""Name"`:                                                    {{int64(20)}},
		},
	}
	r := WrapOscarForDameng(&OscarReader{bridge: f, session: 1, schema: "MiXeD"})
	info, err := r.GetTableDDLInfo(t.Context(), table)
	require.NoError(t, err)
	require.Equal(t, []string{"MiXeD", "SequenceName"}, f.args)
	id := info.Columns[0]
	last := int64(10)
	require.Equal(t, "auto_increment", id.Extra)
	require.Nil(t, id.Default)
	require.Equal(t, &IdentityInfo{Seed: 22, Increment: 3, Last: &last}, id.IdentityInfo)
	require.Equal(t, 3, f.closed)
}

func TestOscarDamengIdentityRejectsExhaustedOrNarrowerSequence(t *testing.T) {
	for _, state := range []oscarSequenceState{
		{start: 1, step: 1, maximum: math.MaxInt32, last: math.MaxInt32, called: true},
		{start: 1, step: 1, maximum: math.MaxInt32 - 1, last: 1},
		{start: 1, step: 3, maximum: math.MaxInt32, last: 1},
	} {
		_, err := oscarDamengIdentitySeed(state, math.MaxInt32, math.MaxInt32)
		require.Error(t, err)
	}
}
