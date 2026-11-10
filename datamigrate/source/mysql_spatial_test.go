package source

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestMySQLSpatialSubtype(t *testing.T) {
	for input, want := range map[string]string{
		"geometry": "Geometry", " POINT ": "Point", "LineString": "LineString", "POLYGON": "Polygon",
		"multipoint": "MultiPoint", "multilinestring": "MultiLineString", "multipolygon": "MultiPolygon",
		"geometrycollection": "GeometryCollection", "geomcollection": "GeometryCollection",
	} {
		t.Run(input, func(t *testing.T) {
			got, ok := MySQLSpatialSubtype(input)
			require.True(t, ok)
			require.Equal(t, want, got)
		})
	}
	for _, input := range []string{"", "varchar", "geography", "pointless", "point(4326)"} {
		_, ok := MySQLSpatialSubtype(input)
		require.False(t, ok, input)
	}
}

func mysqlSpatialColumnRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "DATETIME_PRECISION", "COLUMN_TYPE"}).
		AddRow("AnyShape", "geometry", nil, nil, nil, "YES", nil, "", nil, "geometry").
		AddRow("ZeroPoint", "point", nil, nil, nil, "NO", nil, "", nil, "point").
		AddRow("GeoPoint", "point", nil, nil, nil, "YES", nil, "", nil, "point")
}

const mysqlSpatialSRIDQuery = `SELECT COLUMN_NAME, SRS_ID FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`

func TestMySQLSpatialColumnSRIDPreservesUnrestrictedZeroAndExplicit(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "MixedSchema")
	mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH").WithArgs("MixedSchema", "GeoTable").
		WillReturnRows(mysqlSpatialColumnRows()).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(mysqlSpatialSRIDQuery)).WithArgs("MixedSchema", "GeoTable").
		WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "SRS_ID"}).
			AddRow("AnyShape", nil).AddRow("ZeroPoint", 0).AddRow("GeoPoint", 4326)).RowsWillBeClosed()
	info, err := r.GetTableDDLInfo(context.Background(), "GeoTable")
	require.NoError(t, err)
	require.Equal(t, "GeoTable", info.TableName)
	require.Nil(t, info.Columns[0].SpatialSRID)
	require.Equal(t, "AnyShape", info.Columns[0].Name)
	require.EqualValues(t, 0, *info.Columns[1].SpatialSRID)
	require.False(t, info.Columns[1].IsNullable)
	require.EqualValues(t, 4326, *info.Columns[2].SpatialSRID)
	require.True(t, info.Columns[2].IsNullable)
}

func TestMySQLSpatialSRIDOldVersionOnlyIgnoresMissingSRSIDColumn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		fallback bool
	}{
		{"old-version", &mysql.MySQLError{Number: 1054, Message: "Unknown column 'SRS_ID' in 'field list'"}, true},
		{"permission", &mysql.MySQLError{Number: 1142, Message: "SELECT command denied"}, false},
		{"other-column", &mysql.MySQLError{Number: 1054, Message: "Unknown column 'COLUMN_NAME'"}, false},
		{"connection", errors.New("connection interrupted"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newIndexTestMySQLReader(t, "Src")
			mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH").WithArgs("Src", "MixedTable").WillReturnRows(mysqlSpatialColumnRows())
			mock.ExpectQuery(regexp.QuoteMeta(mysqlSpatialSRIDQuery)).WithArgs("Src", "MixedTable").WillReturnError(tc.err)
			info, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
			if tc.fallback {
				require.NoError(t, err)
				for _, col := range info.Columns {
					require.Nil(t, col.SpatialSRID)
				}
			} else {
				require.ErrorIs(t, err, tc.err)
				require.Nil(t, info)
			}
		})
	}
}

func TestMySQLSpatialSRIDDoesNotAcceptIncompleteMetadata(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery("SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH").WillReturnRows(mysqlSpatialColumnRows())
	mock.ExpectQuery(regexp.QuoteMeta(mysqlSpatialSRIDQuery)).WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "SRS_ID"}).AddRow("AnyShape", nil))
	info, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
	require.ErrorContains(t, err, "SRID 元数据缺失")
	require.Nil(t, info)
}

func TestMySQLSpatialIndexesPreserveOnlyCompleteKeys(t *testing.T) {
	r, mock := newIndexTestMySQLReader(t, "Src")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlIndexesQuery)).WillReturnRows(mysqlIndexRows().
		AddRow("MixedTable", "ValidSpatial", "Location", 1, 32, "SPATIAL", nil).
		AddRow("MixedTable", "ExpressionSpatial", "Before", 1, nil, "SPATIAL", nil).
		AddRow("MixedTable", "ExpressionSpatial", nil, 1, nil, "SPATIAL", nil).
		AddRow("MixedTable", "ExpressionSpatial", "After", 1, nil, "SPATIAL", nil).
		AddRow("MixedTable", "PrefixSpatial", "Before", 1, nil, "SPATIAL", nil).
		AddRow("MixedTable", "PrefixSpatial", "After", 1, 3, "SPATIAL", nil).
		AddRow("MixedTable", "PrefixSpatial", "Final", 1, nil, "SPATIAL", nil).
		AddRow("MixedTable", "MultiSpatial", "A", 1, nil, "SPATIAL", nil).
		AddRow("MixedTable", "MultiSpatial", "B", 1, nil, "SPATIAL", nil))
	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 4)
	require.Equal(t, []string{"Location"}, indexes[0].Columns)
	require.Equal(t, "SPATIAL", indexes[0].Method)
	require.Empty(t, indexes[0].MySQLPrefixLengths, "the fixed 32-byte spatial MBR is not a prefix index")
	require.Equal(t, MySQLSpatialIndexUnsupported, indexes[0].Unsupported)
	require.Equal(t, "MixedTable", indexes[0].TableName)
	require.Nil(t, indexes[0].Descending)
	for _, idx := range indexes[1:] {
		require.NotEmpty(t, idx.Unsupported)
		require.NotEqual(t, MySQLSpatialIndexUnsupported, idx.Unsupported)
		require.Empty(t, idx.Columns, "invalid metadata must never become a usable partial key")
	}
}

func TestIsMySQLSpatialIndex(t *testing.T) {
	for _, method := range []string{"SPATIAL", "RTREE", "rtree", " Spatial "} {
		require.True(t, IsMySQLSpatialIndex(method), method)
	}
	for _, method := range []string{"BTREE", "GIST", "HASH", "FULLTEXT", ""} {
		require.False(t, IsMySQLSpatialIndex(method), method)
	}
}
