package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPartitionDerivedConstraintsUseNativeOwnershipNotTheirNames(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}, raw: map[int64]catalogObject{}, byRaw: map[int64][]int{}, index: map[string]int{}}
		for _, raw := range []catalogObject{
			{id: 10, name: "Root", sub: "UTAB", pid: -1},
			{id: 11, name: "Root_P", sub: "UTAB", pid: 10},
			{id: 20, name: "PK_1_200", sub: "CONS", pid: 11},
			{id: 21, name: "ArbitraryPhysicalCheck", sub: "CONS", pid: 11},
			{id: 22, name: "PK_1_201", sub: "CONS", pid: 10},
		} {
			r.raw[raw.id] = raw
			kind := "constraint"
			if raw.sub == "UTAB" {
				kind = "table"
				if raw.pid > 0 {
					kind = "table_partition"
				}
			}
			r.add(raw.id, Object{Kind: kind, Name: raw.name, Schema: r.schema})
		}
		base := 10
		if mismatch {
			base = 99
		}
		mock.ExpectQuery("SELECT c.ID,t.ID,p.BASE_TABLE_ID FROM SYS.SYSOBJECTS").WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"constraint", "partition", "parent"}).AddRow(20, 11, base).AddRow(21, 11, 10))
		err = r.markPartitionDerivedConstraints()
		if mismatch {
			require.ErrorContains(t, err, "父链")
		} else {
			require.NoError(t, err)
			for _, id := range []int64{20, 21} {
				child := r.base(id)
				require.True(t, child.Managed)
				require.True(t, child.Generated)
				require.Equal(t, r.base(11).ID, child.ParentID)
				require.Equal(t, "Root_P", child.Table)
				require.Contains(t, child.Dependencies, r.base(11).ID)
			}
			require.False(t, r.base(22).Generated, "a root constraint with a similar name is still a logical user object")
		}
		require.NoError(t, mock.ExpectationsWereMet())
		mock.ExpectClose()
		require.NoError(t, db.Close())
	}
}
