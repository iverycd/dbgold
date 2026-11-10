//go:build integration

package cdc

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/target"
	"github.com/stretchr/testify/require"
)

// These DSNs must point to disposable test databases. Only this test's unique
// source table and destination schema are removed during cleanup.
func TestPostgresSpatialCDCIntegration(t *testing.T) {
	sourceDSN, targetDSN := os.Getenv("DBGOLD_MYSQLPG_SPATIAL_SOURCE_DSN"), os.Getenv("DBGOLD_MYSQLPG_SPATIAL_TARGET_DSN")
	if sourceDSN == "" || targetDSN == "" {
		t.Skip("set DBGOLD_MYSQLPG_SPATIAL_SOURCE_DSN and DBGOLD_MYSQLPG_SPATIAL_TARGET_DSN for disposable MySQL/PostGIS databases")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	src, err := sql.Open("mysql", sourceDSN)
	require.NoError(t, err)
	t.Cleanup(func() { src.Close() })
	dst, err := sql.Open("postgres", targetDSN)
	require.NoError(t, err)
	t.Cleanup(func() { dst.Close() })
	name := fmt.Sprintf("spatial_cdc_%d", time.Now().UnixNano())
	sourceName := "`" + name + "`"
	schema := name
	_, err = dst.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema))
	require.NoError(t, err)
	t.Cleanup(func() { _, e := dst.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE"); require.NoError(t, e) })
	caps, err := target.PreparePGSpatial(ctx, dst)
	require.NoError(t, err)
	geometry := quoteIdent(caps.Schema) + ".geometry"
	columns := []string{"Id", "AnyShape", "PointShape", "LineShape", "PolygonShape", "MultiPointShape", "MultiLineShape", "MultiPolygonShape", "CollectionShape"}
	types := []string{"INT", "GEOMETRY", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION"}
	wkts := []string{
		"POINT(12.125 31.25)", "POINT(14.125123456789 33.25)", "LINESTRING(0 0,2 3)",
		"POLYGON((0 0,2 0,2 2,0 0))", "MULTIPOINT((1 2),(3 4))", "MULTILINESTRING((0 0,2 3),(4 5,6 7))",
		"MULTIPOLYGON(((0 0,2 0,2 2,0 0)))", "GEOMETRYCOLLECTION(POINT(1 2),LINESTRING(0 0,2 3))",
	}
	var sourceDefs, targetDefs, quotedSourceColumns, quotedTargetColumns []string
	for i, column := range columns {
		quotedSourceColumns = append(quotedSourceColumns, "`"+column+"`")
		quotedTargetColumns = append(quotedTargetColumns, quoteIdent(column))
		if i == 0 {
			sourceDefs = append(sourceDefs, "`Id` INT PRIMARY KEY")
			targetDefs = append(targetDefs, `"Id" integer PRIMARY KEY`)
		} else {
			sourceDefs = append(sourceDefs, "`"+column+"` "+types[i]+" NULL")
			targetDefs = append(targetDefs, quoteIdent(column)+" "+geometry)
		}
	}
	_, err = src.ExecContext(ctx, "CREATE TABLE "+sourceName+" ("+strings.Join(sourceDefs, ",")+") ENGINE=InnoDB")
	require.NoError(t, err)
	t.Cleanup(func() { _, e := src.Exec("DROP TABLE " + sourceName); require.NoError(t, e) })
	qualified := quoteIdent(schema) + "." + quoteIdent(name)
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+qualified+" ("+strings.Join(targetDefs, ",")+")")
	require.NoError(t, err)
	marks := []string{"1"}
	args := make([]any, 0, len(wkts))
	for _, wkt := range wkts {
		marks = append(marks, "ST_GeomFromText(?,4326)")
		args = append(args, wkt)
	}
	_, err = src.ExecContext(ctx, "INSERT INTO "+sourceName+" VALUES ("+strings.Join(marks, ",")+")", args...)
	require.NoError(t, err)
	readSource := func() []any {
		var id int64
		values := make([][]byte, len(wkts))
		pointers := []any{&id}
		for i := range values {
			pointers = append(pointers, &values[i])
		}
		require.NoError(t, src.QueryRowContext(ctx, "SELECT "+strings.Join(quotedSourceColumns, ",")+" FROM "+sourceName+" WHERE Id=1").Scan(pointers...))
		row := []any{id}
		for _, v := range values {
			if v == nil {
				row = append(row, nil)
			} else {
				row = append(row, v)
			}
		}
		return row
	}
	verify := func(expected []any) {
		for i, column := range columns[1:] {
			q := quoteIdent(column)
			var srid sql.NullInt64
			var wkb []byte
			require.NoError(t, dst.QueryRowContext(ctx, "SELECT "+quoteIdent(caps.Schema)+".ST_SRID("+q+"),"+quoteIdent(caps.Schema)+".ST_AsBinary("+q+",'NDR') FROM "+qualified+` WHERE "Id"=1`).Scan(&srid, &wkb))
			if expected[i+1] == nil {
				require.False(t, srid.Valid)
				require.Nil(t, wkb)
				continue
			}
			raw := expected[i+1].([]byte)
			require.Equal(t, int64(binary.LittleEndian.Uint32(raw[:4])), srid.Int64, column)
			require.True(t, bytes.Equal(raw[4:], wkb), "%s coordinate payload changed", column)
		}
	}
	a, err := NewPostgresApplier(ctx, targetDSN, schema, "spatial-test", false)
	require.NoError(t, err)
	t.Cleanup(func() { a.Close() })
	table := &TableInfo{Name: name, Columns: columns, ColumnTypes: types, PrimaryKey: []int{0}, LocatorStrategy: LocatorPrimaryKey, LocatorColumns: []string{"Id"}}
	position := func(n uint32) Position { return Position{File: "spatial-bin.000001", Pos: n} }
	initial := readSource()
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: table, After: initial}}, position(10))
	require.NoError(t, err)
	verify(initial)
	var nullAssignments []string
	for _, c := range quotedSourceColumns[1:] {
		nullAssignments = append(nullAssignments, c+"=NULL")
	}
	_, err = src.ExecContext(ctx, "UPDATE "+sourceName+" SET "+strings.Join(nullAssignments, ",")+" WHERE Id=1")
	require.NoError(t, err)
	nulls := readSource()
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: table, Before: initial, After: nulls}}, position(20))
	require.NoError(t, err)
	verify(nulls)
	var assignments []string
	for _, c := range quotedSourceColumns[1:] {
		assignments = append(assignments, c+"=ST_GeomFromText(?,0)")
	}
	_, err = src.ExecContext(ctx, "UPDATE "+sourceName+" SET "+strings.Join(assignments, ",")+" WHERE Id=1", args...)
	require.NoError(t, err)
	zeroSRID := readSource()
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: table, Before: nulls, After: zeroSRID}}, position(30))
	require.NoError(t, err)
	verify(zeroSRID)
	_, err = a.Apply(ctx, []Change{{Action: "delete", Table: table, Before: zeroSRID}}, position(40))
	require.NoError(t, err)
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+qualified).Scan(&count))
	require.Zero(t, count)
	bad := append([]any(nil), initial...)
	bad[2] = []byte{0, 0, 0, 0}
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: table, After: bad}}, position(50))
	require.ErrorContains(t, err, "PointShape")
	checkpoint, ok, err := a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, position(40), checkpoint)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+qualified).Scan(&count))
	require.Zero(t, count)
	// A destination DDL after the cached startup check must not silently store
	// canonical geometry text in an ordinary text column or advance the offset.
	_, err = dst.ExecContext(ctx, "ALTER TABLE "+qualified+` ALTER COLUMN "PointShape" TYPE text USING "PointShape"::text`)
	require.NoError(t, err)
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: table, After: initial}}, position(55))
	require.ErrorContains(t, err, "PointShape")
	checkpoint, ok, err = a.LoadCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, position(40), checkpoint)
	require.NoError(t, dst.QueryRowContext(ctx, "SELECT count(*) FROM "+qualified).Scan(&count))
	require.Zero(t, count)

	// These two lines have identical bounding boxes but distinct coordinates.
	// Updating one must not match the other, including on old PostGIS versions.
	keyless := &TableInfo{Name: "Keyless", Columns: []string{"Shape"}, ColumnTypes: []string{"GEOMETRY"}, LocatorStrategy: LocatorFullRow, LocatorColumns: []string{"Shape"}}
	keylessQualified := quoteIdent(schema) + `."Keyless"`
	_, err = dst.ExecContext(ctx, "CREATE TABLE "+keylessQualified+` ("Shape" `+geometry+")")
	require.NoError(t, err)
	shape := func(wkt string) []byte {
		var raw []byte
		require.NoError(t, src.QueryRowContext(ctx, "SELECT ST_GeomFromText(?,0)", wkt).Scan(&raw))
		return raw
	}
	first := shape("LINESTRING(0 0,2 2)")
	second := shape("LINESTRING(0 2,2 0)")
	_, err = a.Apply(ctx, []Change{{Action: "insert", Table: keyless, After: []any{second}}, {Action: "insert", Table: keyless, After: []any{first}}, {Action: "insert", Table: keyless, After: []any{first}}}, position(60))
	require.NoError(t, err)
	_, err = a.Apply(ctx, []Change{{Action: "update", Table: keyless, Before: []any{first}, After: []any{nil}}}, position(70))
	require.NoError(t, err)
	readShapes := func() [][]byte {
		rows, e := dst.QueryContext(ctx, "SELECT "+quoteIdent(caps.Schema)+`.ST_AsBinary("Shape",'NDR') FROM `+keylessQualified)
		require.NoError(t, e)
		defer rows.Close()
		var all [][]byte
		for rows.Next() {
			var b []byte
			require.NoError(t, rows.Scan(&b))
			all = append(all, b)
		}
		require.NoError(t, rows.Err())
		return all
	}
	require.ElementsMatch(t, [][]byte{second[4:], first[4:], nil}, readShapes())
	_, err = a.Apply(ctx, []Change{{Action: "delete", Table: keyless, Before: []any{first}}, {Action: "delete", Table: keyless, Before: []any{nil}}}, position(80))
	require.NoError(t, err)
	require.ElementsMatch(t, [][]byte{second[4:]}, readShapes())
}
