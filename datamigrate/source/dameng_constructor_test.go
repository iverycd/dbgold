package source

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDaMengReaderConstructorPreservesConfiguredOwnerCase(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectPing()
	r, err := newDaMengReader(db, "MiXeD", ConnPoolConfig{})
	require.NoError(t, err)
	mock.ExpectQuery(`SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER = \?`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("RawTable"))
	tables, err := r.ListTables(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"RawTable"}, tables)
	mock.ExpectQuery(`SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE`).WithArgs("MiXeD", "RawTable").WillReturnRows(sqlmock.NewRows([]string{"method", "submethod"}))
	spec, err := r.PartitionMetadata(context.Background(), "RawTable")
	require.NoError(t, err)
	require.Nil(t, spec)
	require.NoError(t, mock.ExpectationsWereMet())
}
