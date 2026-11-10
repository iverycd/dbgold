package typemap

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLPGSpatialMappingsAcrossTargets(t *testing.T) {
	for _, target := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
		t.Run(target, func(t *testing.T) {
			mapper, ok := Get("mysql", target)
			require.True(t, ok)
			for input, expected := range map[string]string{
				"geometry": "geometry", "point": "geometry(Point)", "linestring": "geometry(LineString)", "polygon": "geometry(Polygon)",
				"multipoint": "geometry(MultiPoint)", "multilinestring": "geometry(MultiLineString)", "multipolygon": "geometry(MultiPolygon)",
				"geometrycollection": "geometry(GeometryCollection)", "GEOMCOLLECTION": "geometry(GeometryCollection)",
			} {
				require.Equal(t, expected, mapper(source.ColumnInfo{DataType: input}, false, false), input)
			}
			zero, wgs84 := uint32(0), uint32(4326)
			require.Equal(t, "geometry(Point,0)", mapper(source.ColumnInfo{DataType: "point", SpatialSRID: &zero}, false, false))
			require.Equal(t, "geometry(Geometry,4326)", mapper(source.ColumnInfo{DataType: "geometry", SpatialSRID: &wgs84}, false, false))
		})
	}
}
