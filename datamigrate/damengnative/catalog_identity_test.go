package damengnative

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeColumnIdentityUsesExactColumnAttribute(t *testing.T) {
	ddl := `CREATE TABLE "Mixed"."Base"("Real" BIGINT IDENTITY(10,3) NOT NULL,"identity" INT DEFAULT 1,"Text" VARCHAR(80) DEFAULT 'IDENTITY(1,1)',"Fn" INT DEFAULT identity(1,2),"Generated" INT GENERATED ALWAYS AS("Real"+1) VIRTUAL,CONSTRAINT "identity constraint" CHECK("Real">0)) PARTITION BY RANGE("Real")(PARTITION "P" VALUES LESS THAN(MAXVALUE))`
	for _, c := range []string{"identity", "Text", "Fn", "Generated", "Missing"} {
		found, err := NativeColumnIdentity(ddl, c)
		require.NoError(t, err)
		require.False(t, found, c)
	}
	found, err := NativeColumnIdentity(ddl, "Real")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, declaredNativeIdentity(Object{Units: []Unit{{SQL: ddl, Phase: "before_data"}}}, "Real"))
}

func TestPartitionIdentitySequenceUsesDocumentedActiveIDAndColumn(t *testing.T) {
	info := make([]byte, 50)
	binary.LittleEndian.PutUint32(info[36:40], 167772190)
	binary.LittleEndian.PutUint16(info[40:42], 1)
	binary.LittleEndian.PutUint16(info[42:44], 3)
	id, err := partitionIdentitySequence(info, 3)
	require.NoError(t, err)
	require.EqualValues(t, 167772190, id)
	_, err = partitionIdentitySequence(info, 4)
	require.Error(t, err)
	_, err = partitionIdentitySequence(info[:43], 3)
	require.Error(t, err)
	binary.LittleEndian.PutUint16(info[40:42], 2)
	_, err = partitionIdentitySequence(info, 3)
	require.Error(t, err)
}

func TestPartitionIdentityCurrentReadsOnlyActiveSequenceAndExistingBoundary(t *testing.T) {
	for _, tc := range []struct {
		name          string
		current, step int64
		last          string
		edge          any
		want          int64
		aggregate     string
	}{
		{"allocated", 10, 3, "13", nil, 10, "MAX"},
		{"retained_identity", 10, 3, "13", "13", 13, "MAX"},
		{"explicit_insert", 10, 3, "13", "101", 100, "MAX"},
		{"descending", 10, -3, "7", "-101", -101, "MIN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := &catalogReader{ctx: context.Background(), db: db, schema: "Mixed", raw: map[int64]catalogObject{10: {id: 10, name: "T", sub: "UTAB", pid: -1}, 11: {id: 11, name: "T_Initial", sub: "UTAB", pid: 10}, 167772190: {id: 167772190, name: "Active", sub: "SEQ", info2: 10}, 167772189: {id: 167772189, name: "OldHigherSeed", sub: "SEQ", info2: 10}}}
			info := make([]byte, 50)
			binary.LittleEndian.PutUint32(info[36:40], 167772190)
			binary.LittleEndian.PutUint16(info[40:42], 1)
			m.ExpectQuery(`SELECT t.INFO6,c.COLID`).WithArgs(int64(10), "Id").WillReturnRows(sqlmock.NewRows([]string{"info", "column"}).AddRow(info, 0))
			m.ExpectQuery(`SELECT CAST\(SF_SEQUENCE_GET_LAST_NUM`).WithArgs("Mixed", "Active").WillReturnRows(sqlmock.NewRows([]string{"last"}).AddRow(tc.last))
			m.ExpectQuery(`SELECT CAST\(` + tc.aggregate + `\("Id"\) AS VARCHAR\(80\)\) FROM "Mixed"."T"`).WillReturnRows(sqlmock.NewRows([]string{"edge"}).AddRow(tc.edge))
			got, err := r.partitionIdentityCurrent("T", "Id", tc.current, tc.step)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.NoError(t, m.ExpectationsWereMet(), "历史序列不能被读取或合并")
		})
	}
}
