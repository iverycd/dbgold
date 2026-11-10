package target

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"testing"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func spatialMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return db, mock
}

func expectSpatialType(mock sqlmock.Sqlmock, schema string, extension bool) {
	rows := sqlmock.NewRows([]string{"schema", "postgis", "visible"})
	if schema != "" {
		rows.AddRow(schema, extension, false)
	}
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialTypesSQL)).WillReturnRows(rows).RowsWillBeClosed()
}

func expectSpatialProbe(mock sqlmock.Sqlmock, schema string, valid bool) {
	const ewkb = "0101000020e6100000000000000000f43f00000000000004c0"
	data, _ := hex.DecodeString(ewkb)
	if !valid {
		data[5] = 0
	}
	q := pgSpatialQuote(schema)
	query := `SELECT ` + q + `.ST_SRID(g),` + q + `.ST_GeometryType(g),` + q + `.ST_AsEWKB(g,'NDR') FROM (SELECT $1::` + q + `.geometry AS g) s`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(ewkb).WillReturnRows(sqlmock.NewRows([]string{"srid", "type", "ewkb"}).AddRow(4326, "ST_Point", data))
}

func expectSpatialGIST(mock sqlmock.Sqlmock, schema string, available bool) {
	rows := sqlmock.NewRows([]string{"schema", "opclass"})
	if available {
		rows.AddRow(schema, "gist_geometry_ops_2d")
	}
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialGISTSQL)).WithArgs(schema).WillReturnRows(rows).RowsWillBeClosed()
	if available {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT setting FROM pg_settings WHERE name='enable_astore_gist'`)).WillReturnRows(sqlmock.NewRows([]string{"setting"}))
	}
}

func expectNativeSpatial(mock sqlmock.Sqlmock, schema string, gist bool) {
	expectSpatialType(mock, schema, false)
	expectSpatialProbe(mock, schema, true)
	expectSpatialGIST(mock, schema, gist)
}

func TestPGSpatialReusesNativeSchemaAndSeparatesGIST(t *testing.T) {
	for _, gist := range []bool{false, true} {
		t.Run(map[bool]string{true: "gist", false: "geometry_only"}[gist], func(t *testing.T) {
			db, mock := spatialMock(t)
			expectNativeSpatial(mock, `Geo"Space`, gist)
			caps, err := PreparePGSpatial(context.Background(), db)
			require.NoError(t, err)
			require.Equal(t, `Geo"Space`, caps.Schema)
			require.False(t, caps.ExtensionCreated)
			if gist {
				require.Empty(t, caps.GISTError)
				require.Equal(t, "gist_geometry_ops_2d", caps.GISTOpClass)
			} else {
				require.Contains(t, caps.GISTError, "gist_geometry_ops_2d")
			}
		})
	}
}

func TestPGSpatialEnablesOnlyAvailablePostGIS(t *testing.T) {
	db, mock := spatialMock(t)
	expectSpatialType(mock, "", false)
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialAvailableSQL)).WithArgs("postgis").WillReturnRows(sqlmock.NewRows([]string{"default_version", "installed_version"}).AddRow("3.5", nil))
	mock.ExpectExec(regexp.QuoteMeta(`CREATE EXTENSION IF NOT EXISTS "postgis"`)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectSpatialType(mock, "extensions", true)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT extversion FROM pg_extension WHERE extname=$1`)).WithArgs("postgis").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("3.5"))
	expectSpatialProbe(mock, "extensions", true)
	expectSpatialGIST(mock, "extensions", true)
	caps, err := PreparePGSpatial(context.Background(), db)
	require.NoError(t, err)
	require.True(t, caps.ExtensionCreated)
	require.Equal(t, "postgis", caps.ExtensionName)
	require.Equal(t, "3.5", caps.ExtensionVersion)
	require.Equal(t, "extensions", caps.Schema)
}

func TestPGSpatialReadOnlyInspectionNeverEnablesExtension(t *testing.T) {
	db, mock := spatialMock(t)
	expectSpatialType(mock, "", false)
	_, err := InspectPGSpatial(context.Background(), db)
	require.ErrorContains(t, err, "只读预览")
}

func TestPGSpatialConcurrentPostGISCreationReusesVerifiedExtension(t *testing.T) {
	db, mock := spatialMock(t)
	expectSpatialType(mock, "", false)
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialAvailableSQL)).WithArgs("postgis").WillReturnRows(sqlmock.NewRows([]string{"default_version", "installed_version"}).AddRow("3.5", nil))
	mock.ExpectExec(regexp.QuoteMeta(`CREATE EXTENSION IF NOT EXISTS "postgis"`)).WillReturnError(&pq.Error{Code: "23505", Message: "duplicate extension"})
	expectSpatialType(mock, "gis", true)
	expectSpatialType(mock, "gis", true)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT extversion FROM pg_extension WHERE extname=$1`)).WithArgs("postgis").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("3.5"))
	expectSpatialProbe(mock, "gis", true)
	expectSpatialGIST(mock, "gis", true)
	caps, err := PreparePGSpatial(context.Background(), db)
	require.NoError(t, err)
	require.False(t, caps.ExtensionCreated)
	require.Equal(t, "gis", caps.Schema)
}

func TestPGSpatialRepairColumnMetadataIncludesExplicitZeroConstraint(t *testing.T) {
	db, mock := spatialMock(t)
	expectSpatialColumn(mock, "App", "Shapes", "Shape", "gis", "geometry", 4, "Point", 0, `CHECK (("gis".st_srid("Shape") = 0))`)
	col := source.ColumnInfo{Name: "Shape", DataType: "USER-DEFINED"}
	require.NoError(t, inspectPGSpatialRepairColumn(context.Background(), db, "App", "Shapes", &col))
	require.Equal(t, "geometry", col.DataType)
	require.Equal(t, "gis", col.TypeSchema)
	require.Equal(t, "geometry(Point,0)", col.FormattedType)
	require.NotNil(t, col.SpatialSRID)
	require.Zero(t, *col.SpatialSRID)
}

func TestInspectColumnsResolvesGeometryAfterClosingCursor(t *testing.T) {
	db, mock := spatialMock(t)
	db.SetMaxOpenConns(1)
	mock.ExpectQuery("SELECT column_name,CASE WHEN data_type=").WithArgs("App", "Shapes").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "length", "precision", "scale", "nullable", "default", "extra", "ordinal", "time_precision", "generated"}).AddRow("Shape", "geometry", 0, 0, 0, "YES", nil, "", 1, 0, "")).RowsWillBeClosed()
	expectSpatialColumn(mock, "App", "Shapes", "Shape", "gis", "geometry", 1107460, "Point", 4326)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	columns, err := inspectColumns(ctx, &PostgresWriter{db: db, schema: "App"}, "Shapes")
	require.NoError(t, err)
	require.Len(t, columns, 1)
	require.Equal(t, "gis", columns[0].TypeSchema)
	require.Equal(t, "geometry(Point,4326)", columns[0].FormattedType)
	require.Equal(t, uint32(4326), *columns[0].SpatialSRID)
}

func TestPGSpatialMissingPackageAndPermissionFailure(t *testing.T) {
	for _, permission := range []bool{false, true} {
		t.Run(map[bool]string{true: "permission", false: "missing"}[permission], func(t *testing.T) {
			db, mock := spatialMock(t)
			expectSpatialType(mock, "", false)
			rows := sqlmock.NewRows([]string{"default_version", "installed_version"})
			if permission {
				rows.AddRow("3.5", nil)
			}
			mock.ExpectQuery(regexp.QuoteMeta(pgSpatialAvailableSQL)).WithArgs("postgis").WillReturnRows(rows)
			if permission {
				mock.ExpectExec(regexp.QuoteMeta(`CREATE EXTENSION IF NOT EXISTS "postgis"`)).WillReturnError(&pq.Error{Code: "42501", Message: "permission denied"})
			}
			_, err := PreparePGSpatial(context.Background(), db)
			require.Error(t, err)
			if permission {
				require.Equal(t, "42501", compatibilitySQLState(err))
			} else {
				require.Contains(t, err.Error(), "服务器也未安装")
			}
		})
	}
}

func TestPGSpatialRejectsChangedEWKBAndAmbiguousTypes(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		db, mock := spatialMock(t)
		expectSpatialType(mock, "gis", false)
		expectSpatialProbe(mock, "gis", false)
		_, err := PreparePGSpatial(context.Background(), db)
		require.ErrorContains(t, err, "改变了坐标、类型或 SRID")
	})
	t.Run("ambiguous", func(t *testing.T) {
		db, mock := spatialMock(t)
		mock.ExpectQuery(regexp.QuoteMeta(pgSpatialTypesSQL)).WillReturnRows(sqlmock.NewRows([]string{"schema", "extension", "visible"}).AddRow("one", false, false).AddRow("two", false, false))
		_, err := PreparePGSpatial(context.Background(), db)
		require.ErrorContains(t, err, "多个不可见")
	})
}

func TestPGSpatialGISTTransportFailureIsNotLocalCapabilityFailure(t *testing.T) {
	db, mock := spatialMock(t)
	expectSpatialType(mock, "gis", false)
	expectSpatialProbe(mock, "gis", true)
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialGISTSQL)).WithArgs("gis").WillReturnError(&pq.Error{Code: "08006", Message: "connection lost"})
	_, err := PreparePGSpatial(context.Background(), db)
	require.Error(t, err)
}

func expectSpatialColumn(mock sqlmock.Sqlmock, schema, table, name, typeSchema, dataType string, typmod int64, kind string, srid int64, checks ...string) {
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialColumnSQL)).WithArgs(schema, table, name).WillReturnRows(sqlmock.NewRows([]string{"schema", "type", "typmod"}).AddRow(typeSchema, dataType, typmod))
	if dataType != "geometry" || typeSchema != "gis" {
		return
	}
	if typmod >= 0 {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT "gis".postgis_typmod_type($1),"gis".postgis_typmod_srid($1),"gis".postgis_typmod_dims($1)`)).WithArgs(typmod).WillReturnRows(sqlmock.NewRows([]string{"type", "srid", "dimensions"}).AddRow(kind, srid, 2))
	}
	rows := sqlmock.NewRows([]string{"check"})
	for _, c := range checks {
		rows.AddRow(c)
	}
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialChecksSQL)).WithArgs(schema, table, name).WillReturnRows(rows).RowsWillBeClosed()
}

func TestCheckPGSpatialTablePreservesSRIDDomain(t *testing.T) {
	zero, wgs := uint32(0), uint32(4326)
	for _, tc := range []struct {
		name, source, target string
		sourceSRID           *uint32
		targetSRID           int64
		check                string
		wantError            bool
	}{
		{"unrestricted", "point", "Geometry", nil, 0, "", false},
		{"same", "point", "Point", &wgs, 4326, "", false},
		{"source_zero_target_wgs", "point", "Point", &zero, 4326, "", true},
		{"mixed_source_target_wgs", "point", "Point", nil, 4326, "", true},
		{"mixed_source_target_zero", "point", "Point", nil, 0, `CHECK (("gis".st_srid("Shape") = 0))`, true},
		{"explicit_zero", "point", "Point", &zero, 0, `CHECK (("gis".st_srid("Shape") = 0))`, false},
		{"target_wider", "point", "Geometry", &wgs, 0, "", false},
		{"target_narrower_type", "geometry", "Point", nil, 0, "", true},
		{"unknown_srid_check", "point", "Point", &zero, 0, `CHECK ((st_srid("Shape") IN (0, 4326)))`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := spatialMock(t)
			checks := []string{}
			if tc.check != "" {
				checks = append(checks, tc.check)
			}
			expectSpatialColumn(mock, "App", "Shapes", "Shape", "gis", "geometry", 1107460, tc.target, tc.targetSRID, checks...)
			info := &source.TableDDLInfo{Columns: []source.ColumnInfo{{Name: "Shape", DataType: tc.source, SpatialSRID: tc.sourceSRID}}}
			err := CheckPGSpatialTable(context.Background(), db, "App", "Shapes", info, &PGSpatialCapabilities{Schema: "gis"})
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCheckPGSpatialTableRejectsNativePointAndDifferentGeometry(t *testing.T) {
	for _, dt := range []string{"point", "geometry"} {
		t.Run(dt, func(t *testing.T) {
			db, mock := spatialMock(t)
			expectSpatialColumn(mock, "App", "Shapes", "Shape", "other", dt, -1, "", 0)
			err := CheckPGSpatialTable(context.Background(), db, "App", "Shapes", &source.TableDDLInfo{Columns: []source.ColumnInfo{{Name: "Shape", DataType: "point"}}}, &PGSpatialCapabilities{Schema: "gis"})
			require.ErrorContains(t, err, "不是已验证")
		})
	}
}

func TestPGSpatialCheckSRIDParser(t *testing.T) {
	for _, input := range []string{`CHECK ((st_srid(shape) = 0))`, `CHECK (("Gis"."st_srid"("shape") = 4326)) NOT VALID`} {
		_, err := pgSpatialCheckSRID(input, "shape")
		require.NoError(t, err)
	}
	for _, input := range []string{`CHECK (st_srid(other) = 0)`, `CHECK (st_srid(shape) = 0 OR true)`, `CHECK (st_srid(shape) >= 0)`, `CHECK (st_srid(shape) = 4294967296)`} {
		_, err := pgSpatialCheckSRID(input, "shape")
		require.Error(t, err)
	}
}

func spatialIndex() source.IndexInfo {
	return source.IndexInfo{TableName: "Shapes", IndexName: "ix_shape", Columns: []string{"Shape"}, Method: "gist", PGSpatialSchema: "gis", PGSpatialOpClass: "gist_geometry_ops_2d"}
}

func TestPGSpatialIndexDuplicateMustMatchGISTAndOpClass(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{true: "same", false: "different"}[same], func(t *testing.T) {
			db, mock := spatialMock(t)
			expectNativeSpatial(mock, "gis", true)
			idx := spatialIndex()
			d := dialect.NewPostgres("postgres")
			mock.ExpectExec(regexp.QuoteMeta(dialect.JoinSQL(d.IndexStatements("App", idx)))).WillReturnError(&pq.Error{Code: "42P07", Message: "already exists"})
			method := "gist"
			if !same {
				method = "btree"
			}
			mock.ExpectQuery(regexp.QuoteMeta(pgSpatialIndexSQL)).WithArgs("App", "Shapes", "ix_shape").WillReturnRows(sqlmock.NewRows([]string{"column", "unique", "primary", "valid", "special", "method", "schema", "opclass"}).AddRow("Shape", false, false, true, false, method, "gis", "gist_geometry_ops_2d"))
			err := createPGIndex(context.Background(), db, d, "App", idx)
			if same {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "核验同名索引失败")
			}
		})
	}
}

func TestPGSpatialRepairRejectsSchemaDriftBeforeRawSQL(t *testing.T) {
	db, mock := spatialMock(t)
	expectNativeSpatial(mock, "new_gis", true)
	w := &PostgresWriter{db: db, schema: "App", dia: dialect.NewPostgres("postgres")}
	obj := repair.Object{Kind: "table", Schema: "App", TargetName: "Shapes", Columns: []source.ColumnInfo{{Name: "Shape", DataType: "geometry", TypeSchema: "old_gis"}}}
	units := []repair.Unit{{SQL: `DROP TABLE "App"."Shapes"`, Selected: true}}
	_, err := ExecuteRepair(context.Background(), w, obj, units, nil)
	require.ErrorContains(t, err, "schema 与原修复计划不一致")
}

func TestPGSpatialIndexInspectionDoesNotEnableExtensions(t *testing.T) {
	db, mock := spatialMock(t)
	idx := spatialIndex()
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialIndexSQL)).WithArgs("App", "Shapes", "ix_shape").WillReturnRows(sqlmock.NewRows([]string{"column", "unique", "primary", "valid", "special", "method", "schema", "opclass"}).AddRow("Shape", false, false, true, false, "gist", "gis", "gist_geometry_ops_2d"))
	result, err := inspectRepairIndex(context.Background(), repairTarget{db: db, family: "postgres"}, repair.Object{Kind: "index", Schema: "App", Table: "Shapes", TargetName: "ix_shape", Index: &idx})
	require.NoError(t, err)
	require.Equal(t, "succeeded", result.Status)
}

func TestPGSpatialOrdinaryRepairHasNoSpatialQueries(t *testing.T) {
	db, _ := spatialMock(t)
	require.NoError(t, preparePGSpatialRepair(context.Background(), db, repair.Object{Kind: "table", Columns: []source.ColumnInfo{{Name: "P", DataType: "point"}}}))
	require.NoError(t, preparePGSpatialRepair(context.Background(), db, repair.Object{Kind: "index", Index: &source.IndexInfo{Method: "btree"}}))
}

func TestPGSpatialGISTDisabledIsSeparateFromGeometry(t *testing.T) {
	db, mock := spatialMock(t)
	expectSpatialType(mock, "gis", false)
	expectSpatialProbe(mock, "gis", true)
	mock.ExpectQuery(regexp.QuoteMeta(pgSpatialGISTSQL)).WithArgs("gis").WillReturnRows(sqlmock.NewRows([]string{"schema", "opclass"}).AddRow("gis", "gist_geometry_ops_2d"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT setting FROM pg_settings WHERE name='enable_astore_gist'`)).WillReturnRows(sqlmock.NewRows([]string{"setting"}).AddRow("off"))
	caps, err := PreparePGSpatial(context.Background(), db)
	require.NoError(t, err)
	require.Contains(t, caps.GISTError, "enable_astore_gist")
}

func TestPGSpatialDoesNotSuppressContextCancellation(t *testing.T) {
	db, mock := spatialMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PreparePGSpatial(ctx, db)
	require.True(t, errors.Is(err, context.Canceled))
	require.NoError(t, mock.ExpectationsWereMet())
}
