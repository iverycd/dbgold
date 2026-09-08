package source

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleMySQLMetadataPreservesCase(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "Src", oracleTarget: true}
	m.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("Src", "MixedTable").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "COLUMN_TYPE", "DATETIME_PRECISION", "GENERATION_EXPRESSION"}).AddRow("MixedId", "bigint", nil, 20, 0, "NO", nil, "auto_increment", "bigint unsigned", nil, "").AddRow("CreatedAt", "datetime", nil, nil, nil, "YES", "0000-00-00 00:00:00", "", "datetime(6)", 6, ""))
	info, e := r.GetTableDDLInfo(context.Background(), "MixedTable")
	require.NoError(t, e)
	require.Equal(t, "MixedId", info.Columns[0].Name)
	require.Equal(t, "bigint unsigned", info.Columns[0].FormattedType)
	require.Equal(t, int64(6), info.Columns[1].DateTimePrecision)
	m.ExpectQuery("SELECT TABLE_NAME, AUTO_INCREMENT").WithArgs("Src").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "AUTO_INCREMENT"}).AddRow("MixedTable", "18446744073709551615"))
	m.ExpectQuery("SELECT TABLE_NAME, COLUMN_NAME").WithArgs("Src").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "COLUMN_NAME"}).AddRow("MixedTable", "MixedId"))
	seqs, e := r.GetSequences(context.Background())
	require.NoError(t, e)
	require.Equal(t, "18446744073709551615", seqs[0].StartValueText)
	require.Equal(t, "MixedTable", seqs[0].TableName)
	require.Equal(t, "MixedId", seqs[0].ColumnName)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestOracleMySQLReadPreservesRawBytes(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "Src", oracleTarget: true}
	columns := []*sqlmock.Column{sqlmock.NewColumn("Text").OfType("VARCHAR", []byte{}), sqlmock.NewColumn("When").OfType("DATE", []byte{}), sqlmock.NewColumn("Binary").OfType("BLOB", []byte{})}
	m.ExpectQuery("SELECT \\* FROM `Src`.`select` ORDER BY `MixedId` LIMIT \\? OFFSET \\?").WithArgs(int64(10), int64(0)).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(columns...).AddRow([]byte("a\x00b"), []byte("2024-00-01"), []byte{}))
	cols, types, rows, e := r.ReadPage(context.Background(), "select", []string{"MixedId"}, 0, 10)
	require.NoError(t, e)
	require.Equal(t, []string{"Text", "When", "Binary"}, cols)
	require.Equal(t, "DATE", types[1])
	require.Equal(t, []byte("a\x00b"), rows[0][0])
	require.Equal(t, []byte("2024-00-01"), rows[0][1])
	require.NotNil(t, rows[0][2])
	require.NoError(t, m.ExpectationsWereMet())
}
func TestOracleMySQLRejectsSpecialIndexMetadata(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "Src", oracleTarget: true}
	m.ExpectQuery("SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME").WithArgs("Src").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE", "SUB_PART", "INDEX_TYPE", "COLLATION"}).AddRow("Mixed", "Prefix", "Name", 1, 10, "BTREE", "A").AddRow("Mixed", "Expr", nil, 1, nil, "BTREE", nil))
	indexes, e := r.GetIndexes(context.Background())
	require.NoError(t, e)
	require.Len(t, indexes, 2)
	require.Equal(t, "Mixed", indexes[0].TableName)
	require.NotEmpty(t, indexes[0].Unsupported)
	require.NotEmpty(t, indexes[1].Unsupported)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestOracleMySQLViewsRemainRawWithOriginalColumns(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &MySQLReader{db: db, dbName: "Src", oracleTarget: true}
	raw := "select IFNULL(`Src`.`Mixed`.`Name`,'KeepCase') AS `DisplayName` from `Src`.`Mixed`"
	m.ExpectQuery("SELECT TABLE_NAME, VIEW_DEFINITION").WithArgs("Src").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "VIEW_DEFINITION"}).AddRow("MixedView", raw)).RowsWillBeClosed()
	m.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE").WithArgs("Src", "MixedView").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "COLUMN_TYPE", "DATETIME_PRECISION", "GENERATION_EXPRESSION"}).AddRow("DisplayName", "varchar", 100, nil, nil, "YES", nil, "", "varchar(100)", nil, ""))
	views, e := r.GetViewsForTarget(context.Background(), "oracle")
	require.NoError(t, e)
	require.Equal(t, raw, views[0].Definition)
	require.Equal(t, "MixedView", views[0].ViewName)
	require.Equal(t, "DisplayName", views[0].Columns[0].Name)
	require.NoError(t, m.ExpectationsWereMet())
}
