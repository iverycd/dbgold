package damengnative

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPartitionIdentityPredecessorPreservesSafeProgressionAtIntegerEdges(t *testing.T) {
	for _, tc := range []struct {
		name              string
		next, bound, step int64
		want              int64
		fails             bool
	}{
		{name: "retained13", next: 13, bound: 13, step: 3, want: 13},
		{name: "retained16", next: 19, bound: 16, step: 3, want: 16},
		{name: "explicit_off_progression", next: 13, bound: 101, step: 3, want: 100},
		{name: "descending_off_progression", next: 7, bound: -101, step: -3, want: -101},
		// MAX(existing)+step would overflow in both cases, although an aligned
		// value beyond the existing bound remains representable.
		{name: "maximum_aligned_value", next: math.MaxInt64 - 6, bound: math.MaxInt64 - 2, step: 3, want: math.MaxInt64 - 3},
		{name: "minimum_aligned_value", next: math.MinInt64 + 6, bound: math.MinInt64 + 2, step: -3, want: math.MinInt64 + 3},
		{name: "maximum_stride", next: math.MinInt64, bound: math.MinInt64, step: math.MaxInt64, want: math.MinInt64},
		{name: "minimum_stride", next: math.MaxInt64, bound: 0, step: math.MinInt64, want: math.MaxInt64},
		{name: "ascending_exhausted", next: math.MaxInt64, bound: math.MaxInt64, step: 1, fails: true},
		{name: "descending_exhausted", next: math.MinInt64, bound: math.MinInt64, step: -1, fails: true},
		{name: "zero_stride", next: 10, bound: 10, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := partitionIdentityPredecessor(tc.next, tc.bound, tc.step)
			if tc.fails {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPartitionIdentityCurrentLeavesOrdinaryTablesUnchangedWithoutQueries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "Mixed", raw: map[int64]catalogObject{
		10: {id: 10, name: "T", sub: "UTAB", pid: -1},
		11: {id: 11, name: "TableIndex", sub: "INDEX", pid: 10},
		12: {id: 12, name: "OtherRoot", sub: "UTAB", pid: -1},
		13: {id: 13, name: "OtherChild", sub: "UTAB", pid: 12},
	}}
	for _, step := range []int64{-3, 3} {
		got, err := r.partitionIdentityCurrent("T", "Id", 101, step)
		require.NoError(t, err)
		require.EqualValues(t, 101, got)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPartitionIdentityCurrentRejectsUnprovenActiveSequenceBeforeReadingState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sequence *catalogObject
	}{
		{name: "missing"},
		{name: "different_object_type", sequence: &catalogObject{id: 167772190, name: "WrongType", sub: "UTAB", info2: 10}},
		{name: "different_parent", sequence: &catalogObject{id: 167772190, name: "OtherTableSequence", sub: "SEQ", info2: 99}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := &catalogReader{ctx: context.Background(), db: db, schema: "Mixed", raw: map[int64]catalogObject{
				10: {id: 10, name: "T", sub: "UTAB", pid: -1},
				11: {id: 11, name: "Child", sub: "UTAB", pid: 10},
			}}
			if tc.sequence != nil {
				r.raw[167772190] = *tc.sequence
			}
			info := make([]byte, 44)
			binary.LittleEndian.PutUint32(info[36:40], 167772190)
			binary.LittleEndian.PutUint16(info[40:42], 1)
			mock.ExpectQuery(`SELECT t.INFO6,c.COLID`).WithArgs(int64(10), "Id").WillReturnRows(sqlmock.NewRows([]string{"info", "column"}).AddRow(info, 0))
			_, err = r.partitionIdentityCurrent("T", "Id", 10, 3)
			require.ErrorContains(t, err, "父链")
			require.NoError(t, mock.ExpectationsWereMet(), "unproven objects must never be passed to sequence functions")
		})
	}
}
