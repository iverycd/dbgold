package dialect

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLPGSpatialDDLRetainsRestrictionsAndQualifiesTypes(t *testing.T) {
	zero, wgs84 := uint32(0), uint32(4326)
	info := &source.TableDDLInfo{TableName: "Places", Columns: []source.ColumnInfo{
		{Name: "AnyShape", DataType: "geometry", IsNullable: true},
		{Name: "MixedPoint", DataType: "point", IsNullable: true},
		{Name: "ZeroPoint", DataType: "point", SpatialSRID: &zero},
		{Name: "WGS84", DataType: "point", SpatialSRID: &wgs84, IsNullable: true},
	}}
	for _, target := range []string{"postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase"} {
		t.Run(target, func(t *testing.T) {
			d := NewPostgres(target)
			stmts, err := d.CreateTableStatements("App", info, "mysql", TypeOpt{SpatialSchema: "GIS"}, identity)
			require.NoError(t, err)
			ddl := JoinSQL(stmts)
			require.Contains(t, ddl, `"AnyShape" "GIS"."geometry",`)
			require.Contains(t, ddl, `"MixedPoint" "GIS"."geometry"(Point),`)
			require.Contains(t, ddl, `"ZeroPoint" "GIS"."geometry"(Point,0) NOT NULL CHECK ("GIS"."st_srid"("ZeroPoint") = 0)`)
			require.Contains(t, ddl, `"WGS84" "GIS"."geometry"(Point,4326)`)
			require.Equal(t, 1, strings.Count(ddl, "CHECK ("))
			require.NotContains(t, ddl, `"MixedPoint" point`)
		})
	}
}

func TestMySQLPGSpatialDDLRejectsMissingCapabilitiesAndUnsafeExpressions(t *testing.T) {
	overflow := uint32(1000000)
	for _, tc := range []struct {
		name string
		col  source.ColumnInfo
		opt  TypeOpt
		want string
	}{
		{"missing-schema", source.ColumnInfo{Name: "Shape", DataType: "geometry"}, TypeOpt{}, "空间能力检测"},
		{"default-expression", source.ColumnInfo{Name: "Shape", DataType: "point", Default: strptr("(ST_GeomFromText('POINT(1 2)'))")}, TypeOpt{SpatialSchema: "gis"}, "默认表达式"},
		{"default-string", source.ColumnInfo{Name: "Shape", DataType: "point", Default: strptr("'NULL'")}, TypeOpt{SpatialSchema: "gis"}, "默认表达式"},
		{"virtual", source.ColumnInfo{Name: "Shape", DataType: "point", Extra: "VIRTUAL GENERATED"}, TypeOpt{SpatialSchema: "gis"}, "生成列"},
		{"stored", source.ColumnInfo{Name: "Shape", DataType: "point", Extra: "STORED GENERATED"}, TypeOpt{SpatialSchema: "gis"}, "生成列"},
		{"srid-overflow", source.ColumnInfo{Name: "Shape", DataType: "point", SpatialSRID: &overflow}, TypeOpt{SpatialSchema: "gis"}, "不能原样保留"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := NewPostgres("postgres").CreateTableStatements("app", &source.TableDDLInfo{TableName: "Places", Columns: []source.ColumnInfo{tc.col}}, "mysql", tc.opt, identity)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "Places.Shape")
			require.Empty(t, stmts, "preflight errors must not expose an executable DROP")
		})
	}
}

func TestMySQLPGSpatialDDLAllowsNullDefaultAndEscapesNames(t *testing.T) {
	info := &source.TableDDLInfo{TableName: "Places", Columns: []source.ColumnInfo{{Name: `Odd"Shape`, DataType: "geometry", IsNullable: true, Default: strptr("(NULL)"), Extra: "DEFAULT_GENERATED"}}}
	stmts, err := NewPostgres("postgres").CreateTableStatements("App", info, "mysql", TypeOpt{SpatialSchema: `GI"S`}, identity)
	require.NoError(t, err)
	require.Contains(t, JoinSQL(stmts), `"Odd""Shape" "GI""S"."geometry" DEFAULT NULL`)
}

func TestMySQLPGSpatialIndexSQLUsesVerifiedOperatorClass(t *testing.T) {
	idx := source.IndexInfo{TableName: "Places", IndexName: "Geo", TargetIndexName: `Places_Geo`, Columns: []string{`Odd"Shape`}, Method: "gist", PGSpatialSchema: `GI"S`, PGSpatialOpClass: "gist_geometry_ops_2d"}
	ddl := JoinSQL(NewPostgres("postgres").IndexStatements("App", idx))
	require.Equal(t, `CREATE INDEX "Places_Geo" ON "App"."Places" USING gist ("Odd""Shape" "GI""S"."gist_geometry_ops_2d");`, ddl)
	require.NotContains(t, ddl, "IF NOT EXISTS")
}
