package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// The real DM connection accepts the entire array as one driver argument.
type mysqlDMTestConverter struct{}

func (mysqlDMTestConverter) ConvertValue(v any) (driver.Value, error) { return v, nil }

type mysqlDMTestChunkLength int

func (n mysqlDMTestChunkLength) Match(v driver.Value) bool {
	rows, ok := v.([][]any)
	return ok && len(rows) == int(n)
}

func mockMySQLDMArray(t *testing.T, identity bool) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New(sqlmock.ValueConverterOption(mysqlDMTestConverter{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
	return &DaMengWriter{db: db, schema: "APP", srcType: "mysql", dia: dialect.NewDaMeng(), valueConv: valueconv.NewDaMeng(), identCache: map[string]bool{"T": identity}}, m
}

func TestMySQLDMArrayRowAndByteLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		cols []string
		row  []any
		cap  int
	}{
		{"row cap", []string{"id"}, []any{int64(5)}, mysqlDMBatchRows},
		{"byte cap", []string{"a", "b", "c", "d", "e"}, []any{strings.Repeat("a", 2000), strings.Repeat("b", 2000), strings.Repeat("c", 2000), strings.Repeat("d", 2000), strings.Repeat("e", 2000)}, mysqlDMBatchBytes / (5 * (16 + 2000))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockMySQLDMArray(t, false)
			types := make([]string, len(tc.cols))
			for i := range types {
				types[i] = "VARCHAR"
			}
			rows := make([][]any, tc.cap*2+1)
			for i := range rows {
				rows[i] = tc.row
			}
			m.ExpectBegin()
			for range 2 {
				m.ExpectExec("INSERT INTO").WithArgs(mysqlDMTestChunkLength(tc.cap)).WillReturnResult(sqlmock.NewResult(0, int64(tc.cap)))
			}
			m.ExpectPrepare("INSERT INTO").ExpectExec().WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			require.NoError(t, w.CopyMySQLBatchData(t.Context(), "T", tc.cols, types, rows))
			bulks, scalars := w.BatchWriteStats()
			require.EqualValues(t, 2, bulks)
			require.EqualValues(t, 1, scalars)
		})
	}
}

func TestMySQLDMArrayConversionAndScalarFallback(t *testing.T) {
	w, m := mockMySQLDMArray(t, false)
	cols := []string{"id", "bits", "geom", "dt"}
	types := []string{"BIGINT", "BIT", "GEOMETRY", "DATE"}
	rows := [][]any{
		{uint64(1), nil, nil, nil},
		{uint64(2), []byte{3}, []byte{0, 0, 0, 0, 0, 255}, "0000-00-00"},
		{uint64(3), []byte{4}, nil, nil},
		{uint64(math.MaxUint64), nil, nil, nil},
		{uint64(4), []byte{1}, append([]byte{0, 0, 0, 0}, make([]byte, 2500)...), nil},
	}
	m.ExpectBegin()
	stmt := m.ExpectPrepare("INSERT INTO.*TO_DATE")
	stmt.ExpectExec().WithArgs(uint64(1), nil, nil, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec("INSERT INTO.*TO_DATE").WithArgs([][]any{{uint64(2), int64(3), "00ff", "1900-01-01"}, {uint64(3), int64(4), nil, nil}}).WillReturnResult(sqlmock.NewResult(0, 2))
	stmt.ExpectExec().WithArgs(uint64(math.MaxUint64), nil, nil, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	stmt.ExpectExec().WithArgs(uint64(4), int64(1), strings.Repeat("00", 2500), nil).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	require.NoError(t, w.CopyMySQLBatchData(t.Context(), "T", cols, types, rows))
	bulks, scalars := w.BatchWriteStats()
	require.EqualValues(t, 1, bulks)
	require.EqualValues(t, 3, scalars)
}

func TestMySQLDMArrayLaterFailureRollsBackWholePage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  driver.Result
		execErr error
		reason  string
	}{
		{"short write", sqlmock.NewResult(0, 1), nil, "仅影响 1 行"},
		{"count unavailable", sqlmock.NewErrorResult(errors.New("count unavailable")), nil, "count unavailable"},
		{"constraint", nil, errors.New("constraint failure"), "constraint failure"},
		{"cancelled", nil, context.Canceled, "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := mockMySQLDMArray(t, true)
			m.ExpectBegin()
			m.ExpectExec("SET IDENTITY_INSERT.*ON").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("INSERT INTO").WithArgs(mysqlDMTestChunkLength(1000)).WillReturnResult(sqlmock.NewResult(0, 1000))
			last := m.ExpectExec("INSERT INTO").WithArgs(mysqlDMTestChunkLength(2))
			if tc.execErr != nil {
				last.WillReturnError(tc.execErr)
			} else {
				last.WillReturnResult(tc.result)
			}
			m.ExpectRollback()
			m.ExpectExec("SET IDENTITY_INSERT.*OFF").WillReturnResult(sqlmock.NewResult(0, 0))
			rows := make([][]any, 1002)
			for i := range rows {
				rows[i] = []any{int64(i)}
			}
			err := w.CopyMySQLBatchData(t.Context(), "T", []string{"id"}, []string{"BIGINT"}, rows)
			require.ErrorContains(t, err, tc.reason)
			require.ErrorContains(t, err, "1001–1002")
			if tc.execErr != nil {
				require.ErrorIs(t, err, tc.execErr)
			}
			bulks, scalars := w.BatchWriteStats()
			require.EqualValues(t, 2, bulks)
			require.Zero(t, scalars)
		})
	}
}
