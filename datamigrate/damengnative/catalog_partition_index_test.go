package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPartitionDerivedIndexesRequireNativeParentChain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent int64
		bad    bool
	}{
		{name: "catalog_proven", parent: 10},
		{name: "mismatched_partition_parent", parent: 99, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}, raw: map[int64]catalogObject{}, byRaw: map[int64][]int{}, index: map[string]int{}}
			for _, raw := range []catalogObject{{id: 10, name: "Root", sub: "UTAB", pid: -1}, {id: 11, name: "Root_Low", sub: "UTAB", pid: 10}, {id: 20, name: "UserGlobal", sub: "INDEX", pid: 10}, {id: 21, name: "AnyNativeChildName", sub: "INDEX", pid: 11}, {id: 22, name: "INDEX21_20", sub: "INDEX", pid: 10}} {
				r.raw[raw.id] = raw
				r.add(raw.id, Object{Kind: map[bool]string{true: "table", false: "index"}[raw.sub == "UTAB"], Schema: r.schema, Name: raw.name})
			}
			m.ExpectQuery(`SELECT i.ID,t.ID,p.BASE_TABLE_ID FROM SYS.SYSOBJECTS`).WithArgs("App").WillReturnRows(sqlmock.NewRows([]string{"index", "partition", "parent"}).AddRow(21, 11, tc.parent))
			err = r.markPartitionDerivedIndexes()
			if tc.bad {
				require.ErrorContains(t, err, "父链")
				return
			}
			require.NoError(t, err)
			child := r.base(21)
			require.True(t, child.Managed)
			require.True(t, child.Generated)
			require.Equal(t, r.base(11).ID, child.ParentID)
			require.Equal(t, "Root_Low", child.Table)
			require.False(t, r.base(20).Managed, "user root index is replayed independently")
			require.False(t, r.base(22).Managed, "a generated-looking name on a root table is still a user index")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}
