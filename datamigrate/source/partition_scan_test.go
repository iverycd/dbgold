package source

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestPartitionStreamUsesOneRootResultSetWithBoundedBatches(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT \* FROM "App"\."Root"`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Payload").OfType("BYTEA", []byte{}), sqlmock.NewColumn("Value").OfType("NUMERIC", "")).AddRow([]byte{0, 255}, "12345678901234567890.1234").AddRow([]byte{}, "2").AddRow(nil, "3")).RowsWillBeClosed()
	r := &PostgresReader{db: db, schema: "App"}
	var retained [][][]interface{}
	err := r.StreamPartitionRows(context.Background(), "Root", 2, func(cols, types []string, rows [][]interface{}) error {
		require.Equal(t, []string{"Payload", "Value"}, cols)
		require.Equal(t, []string{"BYTEA", "NUMERIC"}, types)
		retained = append(retained, rows)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, retained, 2)
	require.Len(t, retained[0], 2)
	require.Len(t, retained[1], 1)
	require.Equal(t, []byte{0, 255}, retained[0][0][0])
	require.Equal(t, "12345678901234567890.1234", retained[0][0][1])
}

func TestPartitionStreamClosesOnConsumerFailure(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery(`SELECT \* FROM "App"\."Root"`).WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow(1).AddRow(2)).RowsWillBeClosed()
	err := (&PostgresReader{db: db, schema: "App"}).StreamPartitionRows(context.Background(), "Root", 1, func([]string, []string, [][]interface{}) error { return errors.New("write failed") })
	require.ErrorContains(t, err, "write failed")
}

func TestMySQLPartitionStreamPreservesTextNULAndBinary(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	mock.ExpectQuery("SELECT \\* FROM `App`.`Root`").WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Text").OfType("VARCHAR", ""), sqlmock.NewColumn("Raw").OfType("BLOB", []byte{})).AddRow([]byte("a\x00b"), []byte{0, 255})).RowsWillBeClosed()
	err := (&MySQLReader{db: db, dbName: "App"}).StreamPartitionRows(context.Background(), "Root", 1, func(_ []string, _ []string, rows [][]interface{}) error {
		require.Equal(t, "a\x00b", rows[0][0])
		require.Equal(t, []byte{0, 255}, rows[0][1])
		return nil
	})
	require.NoError(t, err)
}

func TestPartitionLeafCountsUseNativeSelectorsAndKeepEmptyLeaves(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	spec := &PartitionSpec{Method: "RANGE", Partitions: []Partition{{Schema: "App", Name: `Low"Leaf`}, {Schema: "Other", Name: "Empty"}}}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ONLY "App"\."Low""Leaf"`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ONLY "Other"\."Empty"`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	counts, err := CountPartitionRows(context.Background(), db, "postgres", "App", "Root", spec)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{`App.Low"Leaf`: 3, "Other.Empty": 0}, counts)
}

func TestSQLServerLeafCountsUseNativeFunctionAndOrdinalMap(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	spec := &PartitionSpec{Method: "RANGE", Algorithm: "sqlserver-range:Function]Name", Keys: []PartitionKey{{Column: "Key"}}, Partitions: []Partition{{Name: "1"}, {Name: "2"}}}
	mock.ExpectQuery(`SELECT \$PARTITION\.\[Function\]\]Name\]\(\[Key\]\),COUNT_BIG\(\*\) FROM \[Other\]\.\[Root\]`).WillReturnRows(sqlmock.NewRows([]string{"partition", "count"}).AddRow(2, 7))
	counts, err := CountPartitionRows(context.Background(), db, "sqlserver", "Other", "Root", spec)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"1": 0, "2": 7}, counts)
}
