package source

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMySQLScanRangesPreserveFullIntegerDomain(t *testing.T) {
	for _, tc := range []struct {
		name, low, high string
		parts           int
		want            [][2]string
	}{
		{"signed", "-9223372036854775808", "9223372036854775807", 2, [][2]string{{"-9223372036854775808", "-1"}, {"0", "9223372036854775807"}}},
		{"unsigned", "0", "18446744073709551615", 2, [][2]string{{"0", "9223372036854775807"}, {"9223372036854775808", "18446744073709551615"}}},
		{"sparse", "-10", "15", 4, [][2]string{{"-10", "-4"}, {"-3", "3"}, {"4", "10"}, {"11", "15"}}},
		{"narrow", "18446744073709551614", "18446744073709551615", 20, [][2]string{{"18446744073709551614", "18446744073709551614"}, {"18446744073709551615", "18446744073709551615"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			mock.ExpectQuery("SELECT MAX\\(k.COLUMN_NAME\\), MAX\\(c.DATA_TYPE\\).*HAVING COUNT\\(\\*\\)=1").
				WithArgs("App`Db", "Ta`ble").WillReturnRows(sqlmock.NewRows([]string{"name", "type"}).AddRow("Mi`X", "bigint"))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT MIN(`Mi``X`), MAX(`Mi``X`) FROM `App``Db`.`Ta``ble`")).
				WillReturnRows(sqlmock.NewRows([]string{"min", "max"}).AddRow(tc.low, tc.high))
			spans, err := (&MySQLReader{db: db, dbName: "App`Db"}).MySQLScanRanges(context.Background(), "Ta`ble", tc.parts)
			require.NoError(t, err)
			require.Len(t, spans, len(tc.want))
			for i, bounds := range tc.want {
				require.Equal(t, MySQLScanRange{Key: "Mi`X", Lower: bounds[0], Upper: bounds[1]}, spans[i])
			}
		})
	}
}

func TestMySQLScanRangesFallBack(t *testing.T) {
	for _, kind := range []string{"no-single-pk", "text-pk", "empty", "one-reader", "snapshot"} {
		t.Run(kind, func(t *testing.T) {
			db, mock := partitionCatalogMock(t)
			r := &MySQLReader{db: db, dbName: "App"}
			parts := 3
			switch kind {
			case "one-reader":
				parts = 1
			case "snapshot":
				mock.ExpectBegin()
				tx, err := db.Begin()
				require.NoError(t, err)
				r.snapshotTx = tx
				mock.ExpectRollback()
				defer tx.Rollback()
			default:
				keys := sqlmock.NewRows([]string{"key", "type"})
				if kind == "text-pk" {
					keys.AddRow("Key", "varchar")
				} else if kind == "empty" {
					keys.AddRow("Key", "int")
				}
				mock.ExpectQuery("SELECT MAX\\(k.COLUMN_NAME\\).*HAVING COUNT\\(\\*\\)=1").WithArgs("App", "T").WillReturnRows(keys)
				if kind == "empty" {
					mock.ExpectQuery(regexp.QuoteMeta("SELECT MIN(`Key`), MAX(`Key`) FROM `App`.`T`")).WillReturnRows(sqlmock.NewRows([]string{"min", "max"}).AddRow(nil, nil))
				}
			}
			spans, err := r.MySQLScanRanges(context.Background(), "T", parts)
			require.NoError(t, err)
			require.Empty(t, spans)
		})
	}
}

func TestStreamMySQLTableOrdinaryConversionAndRetainedBatches(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	columns := []*sqlmock.Column{
		sqlmock.NewColumn("Txt").OfType("varchar", []byte{}),
		sqlmock.NewColumn("Raw").OfType("BINARY", []byte{}),
		sqlmock.NewColumn("Amount").OfType("DECIMAL", []byte{}),
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM `App``Db`.`Ta``ble`")).
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(columns...).
			AddRow([]byte("a\x00b"), []byte{1, 2}, []byte("12345678901234567890.001")).
			AddRow(nil, []byte{}, []byte("-2")).
			AddRow([]byte("tail"), nil, nil).
			AddRow(nil, []byte(nil), nil)).RowsWillBeClosed()
	var batches [][][]interface{}
	err := (&MySQLReader{db: db, dbName: "App`Db"}).StreamMySQLTable(context.Background(), "Ta`ble", nil, 2, func(cols, types []string, batch [][]interface{}) error {
		require.Equal(t, []string{"Txt", "Raw", "Amount"}, cols)
		require.Equal(t, []string{"VARCHAR", "BINARY", "DECIMAL"}, types)
		batches = append(batches, batch)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, [][][]interface{}{{{"ab", []byte{1, 2}, "12345678901234567890.001"}, {nil, []byte{}, "-2"}}, {{"tail", nil, nil}, {nil, []byte(nil), nil}}}, batches)
}

func TestStreamMySQLTableRawReaderKeepsDriverTypesAndBytes(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &MySQLSQLServerReader{&MySQLReader{db: db, dbName: "App", oracleTarget: true}}
	span := &MySQLScanRange{Key: "Mi`X", Lower: "9223372036854775808", Upper: "18446744073709551615"}
	q := regexp.QuoteMeta("SELECT * FROM `App`.`Ta``ble` WHERE `Mi``X` BETWEEN 9223372036854775808 AND 18446744073709551615")
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Txt").OfType("varchar", []byte{}), sqlmock.NewColumn("At").OfType("DATE", []byte{}), sqlmock.NewColumn("Raw").OfType("BLOB", []byte{})).
		AddRow([]byte("a\x00b"), []byte("2024-00-01"), []byte(nil))
	mock.ExpectPrepare(q).WillBeClosed().ExpectQuery().WillReturnRows(rows).RowsWillBeClosed()
	var batch [][]interface{}
	err := r.StreamMySQLTable(context.Background(), "Ta`ble", span, 1, func(cols, types []string, data [][]interface{}) error {
		require.Equal(t, []string{"Txt", "At", "Raw"}, cols)
		require.Equal(t, []string{"varchar", "DATE", "BLOB"}, types)
		batch = data
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, [][]interface{}{{[]byte("a\x00b"), []byte("2024-00-01"), []byte{}}}, batch)
}

func TestMySQLPartitionStreamKeepsBinaryEmptyDistinctFromNull(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &MySQLReader{db: db, dbName: "App"}
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Raw").OfType("BLOB", []byte{})).
		AddRow([]byte(nil)).AddRow(nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM `App`.`T`")).WillReturnRows(rows).RowsWillBeClosed()
	err := r.StreamPartitionRows(context.Background(), "T", 2, func(_, _ []string, batch [][]interface{}) error {
		require.Equal(t, [][]interface{}{{[]byte{}}, {nil}}, batch)
		return nil
	})
	require.NoError(t, err)
}

func TestStreamMySQLTableClosesResultOnFailure(t *testing.T) {
	db, mock := partitionCatalogMock(t)
	r := &MySQLReader{db: db, dbName: "App"}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM `App`.`T` WHERE `Id` BETWEEN -2 AND 7")).
		WillReturnRows(sqlmock.NewRows([]string{"Id"}).AddRow(-2).AddRow(7)).RowsWillBeClosed()
	writeErr := errors.New("write failed")
	e := r.StreamMySQLTable(context.Background(), "T", &MySQLScanRange{Key: "Id", Lower: "-2", Upper: "7"}, 1,
		func([]string, []string, [][]interface{}) error { return writeErr })
	require.ErrorIs(t, e, writeErr)
	e = r.StreamMySQLTable(context.Background(), "T", &MySQLScanRange{Key: "Id", Lower: "0 OR 1=1", Upper: "7"}, 1,
		func([]string, []string, [][]interface{}) error { return nil })
	require.ErrorContains(t, e, "范围无效")
}
