//go:build integration

package datamigrate

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// The opt-in DSNs must point to disposable integration databases. Each run owns
// unique source tables and a target schema, and never removes shared extensions.
// Leave PostGIS unenabled to exercise the automatic CREATE EXTENSION path.
func TestMySQLPGSpatialIntegration(t *testing.T) {
	sourceDSN, targetDSN := os.Getenv("DBGOLD_MYSQLPG_SPATIAL_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQLPG_SPATIAL_TARGET_DSN")
	if sourceDSN == "" || targetDSN == "" {
		t.Skip("set DBGOLD_MYSQLPG_SPATIAL_SOURCE_DSN and DBGOLD_MYSQLPG_SPATIAL_TARGET_DSN for disposable MySQL/PostGIS databases")
	}
	product := os.Getenv("DBGOLD_MYSQLPG_SPATIAL_PRODUCT")
	if product == "" {
		product = "postgres"
	}
	require.True(t, source.IsPGFamily(product), "unknown PG product %q", product)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	srcConfig, err := mysql.ParseDSN(sourceDSN)
	require.NoError(t, err)
	require.NotEmpty(t, srcConfig.DBName)
	src, err := sql.Open("mysql", sourceDSN)
	require.NoError(t, err)
	defer src.Close()
	driver := "postgres"
	if product == "gaussdb" {
		driver = "opengauss"
	}
	dst, err := sql.Open(driver, targetDSN)
	require.NoError(t, err)
	defer dst.Close()
	require.NoError(t, src.PingContext(ctx))
	require.NoError(t, dst.PingContext(ctx))
	var sourceVersion, targetVersion string
	require.NoError(t, src.QueryRowContext(ctx, "SELECT VERSION()").Scan(&sourceVersion))
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT VERSION()").Scan(&targetVersion))
	t.Logf("source version=%s; target product=%s version=%s", sourceVersion, product, targetVersion)
	var targetSearchPath string
	require.NoError(t, dst.QueryRowContext(ctx, "SHOW search_path").Scan(&targetSearchPath))
	t.Logf("target search_path=%s", targetSearchPath)
	var extensionPreviouslyInstalled bool
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='postgis')").Scan(&extensionPreviouslyInstalled))
	base := fmt.Sprintf("geo_it_%x", time.Now().UnixNano())
	schema := base
	tables := []string{base + "_all", base + "_bad", base + "_ok", base + "_generated", base + "_last"}
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	mq := func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+q(schema))
	require.NoError(t, err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, table := range tables {
			if _, err := src.ExecContext(cleanup, "DROP TABLE IF EXISTS "+mq(table)); err != nil {
				t.Logf("cleanup source %s: %v", table, err)
			}
		}
		if _, err := dst.ExecContext(cleanup, "DROP SCHEMA "+q(schema)+" CASCADE"); err != nil {
			t.Logf("cleanup target %s: %v", schema, err)
		}
	}()
	w, err := newMySQLPGSpatialFixtureWriter(product, targetDSN, schema)
	require.NoError(t, err)
	defer w.Close()
	r, err := source.NewMySQL(sourceDSN, srcConfig.DBName, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	d := w.Dialect()
	qualified := d.QualifyTable(schema, tables[0])
	_, err = src.ExecContext(ctx, "CREATE TABLE "+mq(tables[0])+" ("+
		"Id INT PRIMARY KEY, AnyShape GEOMETRY, Pt POINT, Ln LINESTRING, Poly POLYGON, MP MULTIPOINT, ML MULTILINESTRING, MPoly MULTIPOLYGON, GC GEOMETRYCOLLECTION,"+
		"ZeroPoint POINT SRID 0, IndexedPoint POINT NOT NULL SRID 4326, SPATIAL INDEX geo_location (IndexedPoint)) ENGINE=InnoDB")
	require.NoError(t, err)
	columns := []string{"AnyShape", "Pt", "Ln", "Poly", "MP", "ML", "MPoly", "GC", "ZeroPoint", "IndexedPoint"}
	geometries := []string{
		"POINT(121.12345678912345 31.98765432109876)", "POINT(-10.12345678912345 20.98765432109876)",
		"LINESTRING(120 30,121.12345678912345 31.98765432109876)",
		"POLYGON((120 30,121 30,121 31,120 31,120 30))", "MULTIPOINT((120 30),(121 31))",
		"MULTILINESTRING((120 30,121 31),(122 32,123 33))",
		"MULTIPOLYGON(((120 30,121 30,121 31,120 31,120 30)))",
		"GEOMETRYCOLLECTION(POINT(120 30),GEOMETRYCOLLECTION(LINESTRING(120 30,121 31)))",
		"POINT(-1.12345678912345 -2.98765432109876)", "POINT(121.12345678912345 31.98765432109876)",
	}
	for id := 1; id <= 3; id++ {
		marks := []string{"?"}
		args := []any{id}
		for i, geometry := range geometries {
			if id == 3 && i != 7 && i != 9 {
				marks = append(marks, "NULL")
				continue
			}
			if id == 3 && i == 7 {
				geometry = "GEOMETRYCOLLECTION EMPTY"
			}
			if id == 2 && i == 0 {
				geometry = geometries[3] // a generic column holds a different subtype and SRID.
			}
			srid := 0
			if i == 9 || id == 2 && i != 8 {
				srid = 4326
			}
			if srid == 4326 {
				marks = append(marks, "ST_GeomFromText(?,4326,'axis-order=long-lat')")
			} else {
				marks = append(marks, "ST_GeomFromText(?,0)")
			}
			args = append(args, geometry)
		}
		_, err = src.ExecContext(ctx, "INSERT INTO "+mq(tables[0])+" VALUES ("+strings.Join(marks, ",")+")", args...)
		require.NoError(t, err, "fixture row %d", id)
	}
	indexes, err := r.GetIndexes(ctx)
	require.NoError(t, err)
	for _, idx := range indexes {
		if idx.TableName == tables[0] && idx.IndexName == "geo_location" {
			require.True(t, source.IsMySQLSpatialIndex(idx.Method), "actual source index method %q", idx.Method)
			require.Equal(t, []string{"IndexedPoint"}, idx.Columns, "%+v", idx)
		}
	}
	cfg := Config{Mode: "include", Filter: tables[0], Content: "both", TargetDBType: product, TargetSchema: schema, PageSize: 10, MaxParallel: 1, CaptureRepair: true}
	report := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, cfg).Run(ctx)
	require.Equal(t, 1, report.Tables.Success, "%+v", report.Tables)
	require.Zero(t, report.Tables.Failed, "%+v", report.Tables)
	require.Equal(t, 1, report.Data.Success, "%+v", report.Data)
	require.Zero(t, report.Data.Failed, "%+v", report.Data)
	require.Equal(t, 1, report.Indexes.Success, "%+v", report.Indexes)
	require.Zero(t, report.Indexes.Failed, "%+v", report.Indexes)
	require.NotNil(t, report.SpatialCapabilities)
	cap := report.SpatialCapabilities
	require.Empty(t, cap.GISTError)
	if !extensionPreviouslyInstalled && cap.ExtensionName == "postgis" {
		require.True(t, cap.ExtensionCreated, "the migration itself must enable the available PostGIS extension")
	}
	t.Logf("validated product=%s extension=%s version=%s schema=%s created=%t", product, cap.ExtensionName, cap.ExtensionVersion, cap.Schema, cap.ExtensionCreated)
	assertSpatialRows := func() {
		for id := 1; id <= 3; id++ {
			for _, column := range columns {
				var original []byte
				require.NoError(t, src.QueryRowContext(ctx, "SELECT "+mq(column)+" FROM "+mq(tables[0])+" WHERE Id=?", id).Scan(&original))
				var srid sql.NullInt64
				var wkb []byte
				statement := fmt.Sprintf("SELECT %s.st_srid(%s),%s.st_asbinary(%s,'NDR') FROM %s WHERE %s=$1", q(cap.Schema), q(column), q(cap.Schema), q(column), qualified, q("Id"))
				require.NoError(t, dst.QueryRowContext(ctx, statement, id).Scan(&srid, &wkb), "%s row %d", column, id)
				if original == nil {
					require.False(t, srid.Valid, "%s row %d", column, id)
					require.Nil(t, wkb)
					continue
				}
				require.GreaterOrEqual(t, len(original), 9)
				require.Equal(t, byte(1), original[4], "MySQL internal WKB uses little-endian in this fixture")
				require.True(t, srid.Valid)
				require.EqualValues(t, binary.LittleEndian.Uint32(original[:4]), srid.Int64, "%s row %d SRID", column, id)
				require.Equal(t, original[4:], wkb, "%s row %d precision, axis order and nested geometry must survive exactly", column, id)
			}
		}
	}
	assertSpatialRows()
	_, err = dst.ExecContext(ctx, "UPDATE "+qualified+" SET "+q("ZeroPoint")+"='SRID=4326;POINT(1 2)'::"+q(cap.Schema)+".geometry WHERE "+q("Id")+"=1")
	require.Error(t, err, "explicit SRID 0 remains a CHECK restriction")

	require.NotNil(t, report.RepairManifest)
	var object *repair.Object
	var tableObject *repair.Object
	for _, item := range report.RepairManifest.Objects {
		if item.Kind == "table" && item.SourceTable == tables[0] {
			copy := item
			tableObject = &copy
		}
		if item.Kind == "index" && item.SourceName == "geo_location" && item.SourceTable == tables[0] {
			copy := item
			object = &copy
		}
	}
	require.NotNil(t, tableObject)
	// Recovery previews validate the same schema-qualified geometry/SRID
	// metadata without enabling extensions. The explicit truncate is confined
	// to this test-owned table and reloads all spatial values through COPY.
	tableObject.DataStatus = "failed"
	preview, err := PreviewRecovery(ctx, r, w, cfg, *tableObject, nil, tableObject.ColumnMapping)
	require.NoError(t, err)
	require.True(t, preview.DataRequired)
	require.EqualValues(t, 3, preview.SourceRows)
	require.EqualValues(t, 3, preview.TargetRows)
	recovered, err := RunRecovery(ctx, r, w, cfg, preview, true, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", recovered.DataStatus)
	require.EqualValues(t, 3, recovered.CommittedRows)
	assertSpatialRows()
	require.NotNil(t, object)
	require.Equal(t, "gist", object.Index.Method)
	verified, err := target.InspectRepair(ctx, w, *object)
	require.NoError(t, err)
	require.Equal(t, "succeeded", verified.Status, "%+v", verified)
	require.NoError(t, w.CreateIndex(ctx, *object.Index), "same verified GiST definition can be reused")
	_, err = dst.ExecContext(ctx, "DROP INDEX "+d.QualifyTable(schema, object.TargetName))
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "CREATE INDEX "+q(object.TargetName)+" ON "+qualified+" ("+q("Id")+")")
	require.NoError(t, err)
	verified, err = target.InspectRepair(ctx, w, *object)
	require.NoError(t, err)
	require.NotEqual(t, "succeeded", verified.Status, "same-name BTREE cannot count as the spatial index")
	require.Error(t, w.CreateIndex(ctx, *object.Index), "wrong same-name definition cannot be reused")
	_, err = dst.ExecContext(ctx, "DROP INDEX "+d.QualifyTable(schema, object.TargetName))
	require.NoError(t, err)
	units := append([]repair.Unit(nil), object.Units...)
	for i := range units {
		units[i].Selected, units[i].Status = true, "failed"
	}
	units, err = target.ExecuteRepair(ctx, w, *object, units, nil)
	require.NoError(t, err, "spatial repair replays the qualified GiST plan")
	for _, unit := range units {
		require.Equal(t, "succeeded", unit.Status)
	}
	verified, err = target.InspectRepair(ctx, w, *object)
	require.NoError(t, err)
	require.Equal(t, "succeeded", verified.Status)
	cfg.Content, cfg.Objects = "schema_only", []string{"indexes"}
	retried := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, cfg).Run(ctx)
	require.Equal(t, 1, retried.Indexes.Success, "%+v", retried.Indexes)
	require.Zero(t, retried.Indexes.Failed, "%+v", retried.Indexes)
	require.NotNil(t, retried.RepairManifest)
	verifiedTableDependency := false
	for _, item := range retried.RepairManifest.Objects {
		if item.Kind == "table" && item.SourceTable == tables[0] {
			check, err := target.InspectRepair(ctx, w, item)
			require.NoError(t, err)
			require.Equal(t, "succeeded", check.Status, "indexes-only table dependency: %+v", check)
			verifiedTableDependency = true
		}
	}
	require.True(t, verifiedTableDependency, "indexes-only repair must retain a verifiable spatial table dependency")

	// A data-only mismatch must not alter the incompatible table, and must not
	// stop an independent ordinary table that follows it in source order.
	_, err = src.ExecContext(ctx, "CREATE TABLE "+mq(tables[1])+" (Id INT PRIMARY KEY, Shape POINT)")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+mq(tables[1])+" VALUES (1,POINT(1,2))")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "CREATE TABLE "+mq(tables[2])+" (Id INT PRIMARY KEY, Payload VARCHAR(20))")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+mq(tables[2])+" VALUES (1,'independent')")
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+d.QualifyTable(schema, tables[1])+" ("+q("Id")+" int PRIMARY KEY,"+q("Shape")+" text)")
	require.NoError(t, err)
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+d.QualifyTable(schema, tables[2])+" ("+q("Id")+" int PRIMARY KEY,"+q("Payload")+" varchar(20))")
	require.NoError(t, err)
	cfg.Content, cfg.Objects, cfg.Filter = "data_only", nil, strings.Join(tables[1:3], ",")
	dataReport := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, cfg).Run(ctx)
	require.Equal(t, 1, dataReport.Data.Success, "%+v", dataReport.Data)
	require.Equal(t, 1, dataReport.Data.Failed, "%+v", dataReport.Data)
	var badCount, goodCount int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+d.QualifyTable(schema, tables[1])).Scan(&badCount))
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+d.QualifyTable(schema, tables[2])).Scan(&goodCount))
	require.Zero(t, badCount)
	require.Equal(t, 1, goodCount)

	// The journal test also uses this deliberate failure. Its behavior must be
	// stable with PostGIS already enabled, while the next ordinary table loads.
	_, err = src.ExecContext(ctx, "CREATE TABLE "+mq(tables[3])+" (Id INT PRIMARY KEY, Shape GEOMETRY GENERATED ALWAYS AS (POINT(Id,Id)) STORED)")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+mq(tables[3])+" (Id) VALUES (1)")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "CREATE TABLE "+mq(tables[4])+" (Id INT PRIMARY KEY)")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx, "INSERT INTO "+mq(tables[4])+" VALUES (1)")
	require.NoError(t, err)
	cfg.Content, cfg.Filter = "both", strings.Join(tables[3:], ",")
	partial := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, cfg).Run(ctx)
	require.Equal(t, 1, partial.Tables.Failed, "%+v", partial.Tables)
	require.Equal(t, 1, partial.Tables.Success, "%+v", partial.Tables)
	require.Equal(t, 1, partial.Data.Success, "%+v", partial.Data)
	require.Contains(t, partial.Tables.Items[0].Error, "空间生成列表达式不支持自动转换")
	var generatedExists bool
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2)", schema, tables[3]).Scan(&generatedExists))
	require.False(t, generatedExists, "preflight rejects the unsupported table before any target table DDL")
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+d.QualifyTable(schema, tables[4])).Scan(&goodCount))
	require.Equal(t, 1, goodCount)
}

func newMySQLPGSpatialFixtureWriter(product, dsn, schema string) (target.Writer, error) {
	pool := target.ConnPoolConfig{}
	switch product {
	case "gaussdb":
		return target.NewGaussDB(dsn, schema, pool)
	case "kingbase":
		return target.NewKingbase(dsn, schema, pool)
	case "highgo":
		return target.NewHighGo(dsn, schema, pool)
	case "vastbase":
		return target.NewVastbase(dsn, schema, pool)
	case "gbase":
		return target.NewGBase(dsn, schema, pool)
	default:
		return target.NewPostgresCompatible(dsn, schema, product, pool)
	}
}
