package source

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMySQLLinearHashCatalogFreezesOriginalNamesAndNativeBucketOrder(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT PARTITION_NAME,PARTITION_METHOD`).WithArgs("MiXeD", "Events").WillReturnRows(mysqlPartitionCatalogRows().
		AddRow("ThirdName", "LINEAR HASH", "`MiXeDKey`", nil, 1, nil, nil, nil, nil).
		AddRow("FirstName", "LINEAR HASH", "`MiXeDKey`", nil, 2, nil, nil, nil, nil).
		AddRow("SecondName", "LINEAR HASH", "`MiXeDKey`", nil, 3, nil, nil, nil, nil)).RowsWillBeClosed()
	mock.ExpectQuery(`SELECT COLUMN_NAME,COLUMN_TYPE,COLLATION_NAME`).WithArgs("MiXeD", "Events").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "collation"}).AddRow("MiXeDKey", "bigint", nil)).RowsWillBeClosed()
	mock.ExpectQuery("SHOW CREATE TABLE `MiXeD`.`Events`").WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Events", "CREATE TABLE `Events` (`MiXeDKey` BIGINT) PARTITION BY LINEAR HASH (`MiXeDKey`) (PARTITION `ThirdName`,PARTITION `FirstName`,PARTITION `SecondName`)"))
	spec, err := (&MySQLReader{db: db, dbName: "MiXeD"}).PartitionMetadata(context.Background(), "Events")
	require.NoError(t, err)
	require.Empty(t, spec.Unsupported)
	require.Equal(t, "LINEAR HASH", spec.Method)
	require.Equal(t, "mysql-linear-hash", spec.Algorithm)
	require.Equal(t, "MiXeDKey", spec.Keys[0].Column)
	for i, name := range []string{"ThirdName", "FirstName", "SecondName"} {
		require.Equal(t, name, spec.Partitions[i].Name)
		require.Equal(t, "MiXeD", spec.Partitions[i].Schema)
		require.Equal(t, 3, spec.Partitions[i].HashModulus)
		require.Equal(t, i, spec.Partitions[i].HashRemainder)
	}
}
