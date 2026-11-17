package source

import (
	"context"
	"errors"
	"io"
	"testing"

	"dbgold/datamigrate/validation"
	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

type oscarFixtureFetch struct {
	rows [][]any
	done bool
}

type oscarReaderFixture struct {
	oscarReadBridge
	cols            []jdbcbridge.MigrationColumn
	queries         map[string][][]any
	fetches         []oscarFixtureFetch
	opened          int
	closed          int
	query           string
	args            []string
	closeContextErr error
}

func (f *oscarReaderFixture) MigrationColumns(_ context.Context, _ uint64, schema, table string) ([]jdbcbridge.MigrationColumn, error) {
	if schema != "MiXeD" || table != `Table"Name` {
		return nil, errors.New("unexpected metadata identity")
	}
	return append([]jdbcbridge.MigrationColumn(nil), f.cols...), nil
}
func (f *oscarReaderFixture) OpenCursor(_ context.Context, _ uint64, query string) (uint64, error) {
	f.opened++
	f.query = query
	return 1, nil
}
func (f *oscarReaderFixture) OpenBoundCursor(ctx context.Context, id uint64, query string, args ...string) (uint64, error) {
	f.args = append([]string(nil), args...)
	return f.OpenCursor(ctx, id, query)
}
func (f *oscarReaderFixture) FetchCursor(_ context.Context, _ uint64, _, _ int) ([][]any, bool, error) {
	rows, found := f.queries[f.query]
	if !found {
		return nil, false, errors.New("unavailable catalog query")
	}
	return rows, true, nil
}
func (f *oscarReaderFixture) FetchMigrationCursor(ctx context.Context, _ uint64, _, _ int) ([][]any, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	first := f.fetches[0]
	f.fetches = f.fetches[1:]
	return first.rows, first.done, nil
}
func (f *oscarReaderFixture) CloseCursor(ctx context.Context, _ uint64) error {
	f.closed++
	f.closeContextErr = ctx.Err()
	return nil
}

func TestOscarReaderStreamsAllShortChunksAndKeepsNames(t *testing.T) {
	fixture := &oscarReaderFixture{
		cols: []jdbcbridge.MigrationColumn{
			{Name: "payload", Type: "BLOB", Ordinal: 2},
			{Name: "Quoted Id", Type: "DECIMAL", Ordinal: 1},
		},
		fetches: []oscarFixtureFetch{
			{rows: [][]any{{"12345678901234567890.01", []byte{0, 1}}}},
			{rows: [][]any{{nil, []byte{}}, {"1.25", nil}}, done: true},
		},
	}
	r := &OscarReader{bridge: fixture, session: 1, schema: "MiXeD"}
	var result [][]any
	err := r.StreamOscarTable(t.Context(), `Table"Name`, 5, func(cols, types []string, rows [][]any) error {
		require.Equal(t, []string{"Quoted Id", "payload"}, cols)
		require.Equal(t, []string{"DECIMAL", "BLOB"}, types)
		result = append(result, rows...)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, [][]any{{"12345678901234567890.01", []byte{0, 1}}, {nil, []byte{}}, {"1.25", nil}}, result)
	require.Equal(t, `SELECT "Quoted Id","payload" FROM "MiXeD"."Table""Name"`, fixture.query)
	require.Equal(t, 1, fixture.opened)
	require.Equal(t, 1, fixture.closed)
}

func TestOscarReaderClosesMigrationCursorAfterCancellation(t *testing.T) {
	fixture := &oscarReaderFixture{
		cols:    []jdbcbridge.MigrationColumn{{Name: "Id", Type: "INT", Ordinal: 1}},
		fetches: []oscarFixtureFetch{{rows: [][]any{{int64(0)}}}},
	}
	r := &OscarReader{bridge: fixture, session: 1, schema: "MiXeD"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := r.StreamOscarTable(ctx, `Table"Name`, 5, func(_, _ []string, _ [][]any) error {
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, fixture.closed)
	require.NoError(t, fixture.closeContextErr)
}

func TestOscarReaderValidationPreservesBitIdentityAndUsesBoundPrimaryKeys(t *testing.T) {
	pkQuery := `SELECT TABLE_NAME,PK_NAME,COLUMN_NAME,KEY_SEQ FROM V_SYS_PRIMARY_KEYS WHERE TABLE_SCHEM=? AND TABLE_NAME=? ORDER BY TABLE_NAME,PK_NAME,KEY_SEQ`
	fixture := &oscarReaderFixture{
		cols: []jdbcbridge.MigrationColumn{{Name: "Bit", Type: "BIT", Size: 1, Ordinal: 1}},
		queries: map[string][][]any{
			pkQuery: {{`Table"Name`, "PK", "Bit", int64(1)}},
			`SELECT "Bit" FROM "MiXeD"."Table""Name"`: {{true}, {false}, {nil}},
		},
	}
	r := &OscarReader{bridge: fixture, session: 1, schema: "MiXeD"}
	meta, err := r.DescribeValidationTable(t.Context(), `Table"Name`)
	require.NoError(t, err)
	require.Equal(t, []string{"MiXeD", `Table"Name`}, fixture.args)
	require.Equal(t, []string{"Bit"}, meta.PrimaryKey)
	require.Equal(t, validation.Bits, meta.Columns[0].Kind)
	scan, err := r.OpenValidationScan(t.Context(), `Table"Name`, meta.Columns)
	require.NoError(t, err)
	defer scan.Close()
	for _, v := range []any{"1", "0", nil} {
		row, err := scan.Next(t.Context())
		require.NoError(t, err)
		require.Equal(t, []any{v}, row)
	}
	_, err = scan.Next(t.Context())
	require.ErrorIs(t, err, io.EOF)
}

func TestOscarSequenceReferenceRetainsQuotedCase(t *testing.T) {
	owner, name := oscarSequenceDefault(`nextval('"MiXeD"."Seq""Name"'::regclass)`)
	require.Equal(t, "MiXeD", owner)
	require.Equal(t, `Seq"Name`, name)
	owner, name = oscarSequenceDefault(`nextval('app.seq')`)
	require.Equal(t, "APP", owner)
	require.Equal(t, "SEQ", name)
	_, name = oscarSequenceDefault(`nextval('app.seq')+1`)
	require.Empty(t, name)
}
