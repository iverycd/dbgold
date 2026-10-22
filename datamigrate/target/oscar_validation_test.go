package target

import (
	"context"
	"io"
	"testing"

	"dbgold/internal/jdbcbridge"
	"github.com/stretchr/testify/require"
)

type fakeValidationOscarBridge struct {
	fakeOscarBridge
	fetches, closes       int
	bytesLimit, rowsLimit int
}

func (f *fakeValidationOscarBridge) DescribeTable(context.Context, uint64, string, string) (jdbcbridge.TableMetadata, error) {
	return jdbcbridge.TableMetadata{Columns: []jdbcbridge.ColumnMetadata{{Name: "Id", TypeName: "BIGINT", Precision: 19, Length: 19}, {Name: "Original", TypeName: "CLOB", Length: 123456}}, PrimaryKey: []string{"Id"}}, nil
}
func (f *fakeValidationOscarBridge) OpenCursor(_ context.Context, _ uint64, q string) (uint64, error) {
	f.lastSQL = q
	return 3, nil
}
func (f *fakeValidationOscarBridge) FetchCursor(_ context.Context, _ uint64, maxRows, maxBytes int) ([][]any, bool, error) {
	f.fetches++
	f.bytesLimit = maxBytes
	f.rowsLimit = maxRows
	if f.fetches == 1 {
		return [][]any{{int64(1), "x\x00y "}}, false, nil
	}
	return [][]any{{int64(2), "z"}}, true, nil
}
func (f *fakeValidationOscarBridge) CloseCursor(context.Context, uint64) error {
	f.closes++
	return nil
}
func TestOscarValidationReadsBoundedPagesAndCloses(t *testing.T) {
	f := &fakeValidationOscarBridge{}
	w := newFakeOscarWriter(f)
	meta, err := w.DescribeValidationTable(context.Background(), "Mixed")
	require.NoError(t, err)
	stream, err := w.OpenValidationScan(context.Background(), "Mixed", meta.Columns)
	require.NoError(t, err)
	require.Contains(t, f.lastSQL, `SELECT "Id","Original" FROM "业务"."Mixed"`)
	row, err := stream.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, "x\x00y ", row[1])
	_, err = stream.Next(context.Background())
	require.NoError(t, err)
	_, err = stream.Next(context.Background())
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 8<<20, f.bytesLimit)
	require.Equal(t, 128, f.rowsLimit)
	require.NoError(t, stream.Close())
	require.NoError(t, stream.Close())
	require.Equal(t, 1, f.closes)
}
func TestOscarValidationDoesNotRequireNewMigrationBridgeMethods(t *testing.T) {
	_, err := newFakeOscarWriter(&fakeOscarBridge{}).DescribeValidationTable(context.Background(), "T")
	require.ErrorContains(t, err, "does not support lossless")
}
