package datamigrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"gitee.com/chunanyong/dm"
	"github.com/stretchr/testify/require"
)

// BFILE is a native attribute, not a heuristic on strings containing ':'. This
// fixture combines direct attributes, nested objects, scalar/object collections,
// nested collections, NULLs and unrelated locator-shaped text in a whole-schema
// migration. External directories/files must already be prepared on both sides.
func TestDamengNativeIntegrationNestedBFileResources(t *testing.T) {
	sd, ss := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dd, ds := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	directory, filename := os.Getenv("DAMENG_NATIVE_BFILE_DIRECTORY"), os.Getenv("DAMENG_NATIVE_BFILE_FILENAME")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || sd == "" || ss == "" || dd == "" || ds == "" || directory == "" || filename == "" {
		t.Skip("需要两个预建空DM8 Schema、SOURCE/TARGET_DSN/SCHEMA、ALLOW_DDL=1及两端预备的BFILE_DIRECTORY/BFILE_FILENAME")
	}
	ctx, stop := context.WithTimeout(context.Background(), 4*time.Minute)
	defer stop()
	src, err := sql.Open("dm", sd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	dst, err := sql.Open("dm", dd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dst.Close() })
	ss, err = damengnative.ResolveSchema(ctx, src, ss)
	require.NoError(t, err)
	ds, err = damengnative.ResolveSchema(ctx, dst, ds)
	require.NoError(t, err)
	for _, side := range []struct {
		db     *sql.DB
		schema string
	}{{src, ss}, {dst, ds}} {
		var count int
		require.NoError(t, side.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, side.schema).Scan(&count))
		require.Zero(t, count, "需要初始为空的独立测试Schema")
		require.NoError(t, damengnative.CheckBFileResource(ctx, side.db, directory+":"+filename))
	}
	sq := func(n string) string { return damengnative.Qualified(ss, n) }
	dq := func(n string) string { return damengnative.Qualified(ds, n) }
	const inner, files, items, matrix, outer, table = `File.Item"Type`, "FileList", "ItemList", "FileMatrix", "FileEnvelope", "NestedFiles"
	const staticFiles, dynamicFiles, nativeArrays = "StaticFiles", "DynamicFiles", "NativeArrays"
	var frozen *damengnative.Preflight
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if frozen != nil {
			objects := damengnative.Order(frozen.Objects)
			for i := len(objects) - 1; i >= 0; i-- {
				if !objects[i].Managed && objects[i].DropSQL != "" {
					_, _ = dst.ExecContext(clean, objects[i].DropSQL)
				}
			}
		}
		_, _ = src.ExecContext(clean, "DROP TABLE "+sq(table)+" CASCADE CONSTRAINTS")
		for _, n := range []string{nativeArrays, dynamicFiles, staticFiles, outer, matrix, items, files, inner} {
			_, _ = src.ExecContext(clean, "DROP TYPE "+sq(n))
		}
	})
	exec := func(db *sql.DB, text string, args ...any) {
		_, e := db.ExecContext(ctx, text, args...)
		require.NoError(t, e, text)
	}
	for _, ddl := range []string{
		"CREATE TYPE " + sq(inner) + ` AS OBJECT("File" BFILE,"PlainText" VARCHAR(512),"Body" CLOB,"Amount" DECIMAL(38,19),"Payload" BLOB,"When" TIMESTAMP(9) WITH TIME ZONE,"Duration" INTERVAL DAY(3) TO SECOND(6))`,
		"CREATE TYPE " + sq(files) + ` AS VARRAY(5) OF BFILE`,
		"CREATE TYPE " + sq(items) + " AS VARRAY(5) OF " + sq(inner),
		"CREATE TYPE " + sq(matrix) + " AS VARRAY(5) OF " + sq(files),
		"CREATE TYPE " + sq(outer) + ` AS OBJECT("Item" ` + sq(inner) + `,"Files" ` + sq(files) + `,"Items" ` + sq(items) + `,"Matrix" ` + sq(matrix) + `)`,
		"CREATE TABLE " + sq(table) + `("Id" INT PRIMARY KEY,"Object" ` + sq(outer) + `,"Files" ` + sq(files) + `)`,
	} {
		exec(src, ddl)
	}
	file := "BFILENAME(?,?)"
	const amount = "-1234567890123456789.1234567890123456789"
	item := sq(inner) + "(" + file + ",?,?," + amount + ",HEXTORAW('0001FF00'),TO_TIMESTAMP_TZ('2026-10-26 12:34:56.123456789 +05:45','YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM'),INTERVAL '123 01:02:03.123456' DAY(3) TO SECOND(6))"
	object := sq(outer) + "(" + item + "," + sq(files) + "(" + file + ",NULL)," + sq(items) + "(" + item + ",NULL)," + sq(matrix) + "(" + sq(files) + "(" + file + ",NULL),NULL," + sq(files) + "()))"
	large := strings.Repeat("中文😀\x00abc", 20000)
	exec(src, "INSERT INTO "+sq(table)+" VALUES(1,"+object+","+sq(files)+"("+file+",NULL))", directory, filename, "NoSuchDir:not:a:file", dm.NewClob(large), directory, filename, directory, filename, "not a locator:", dm.NewClob("small\x00body"), directory, filename, directory, filename)
	exec(src, "INSERT INTO "+sq(table)+" VALUES(2,"+sq(outer)+"("+sq(inner)+"(NULL,'Nonexistent:file',EMPTY_CLOB(),NULL,EMPTY_BLOB(),TO_TIMESTAMP_TZ('2026-10-26 23:59:59.999999999 -03:30','YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM'),NULL),"+sq(files)+"(),"+sq(items)+"(),"+sq(matrix)+"()),"+sq(files)+"())")
	exec(src, "INSERT INTO "+sq(table)+" VALUES(3,"+sq(outer)+"(NULL,NULL,NULL,NULL),NULL)")
	exec(src, "INSERT INTO "+sq(table)+" VALUES(4,NULL,NULL)")
	exec(src, "CREATE TYPE "+sq(staticFiles)+" AS ARRAY BFILE[5]")
	exec(src, "CREATE TYPE "+sq(dynamicFiles)+" AS ARRAY BFILE[]")
	exec(src, "CREATE TYPE "+sq(nativeArrays)+` AS OBJECT("Static" `+sq(staticFiles)+`,"Dynamic" `+sq(dynamicFiles)+`,"Tail" VARCHAR(50))`)
	exec(src, "ALTER TABLE "+sq(table)+` ADD COLUMN "NativeArray" `+sq(nativeArrays))
	exec(src, "DECLARE a "+sq(staticFiles)+"; b "+sq(dynamicFiles)+"; BEGIN a[1]:=BFILENAME(?,?); b:=NEW BFILE[3]; b[1]:=BFILENAME(?,?); b[3]:=BFILENAME(?,?); UPDATE "+sq(table)+` SET "NativeArray"=`+sq(nativeArrays)+`(a,b,'after native arrays') WHERE "Id"=1; END;`, directory, filename, directory, filename, directory, filename)
	exec(src, "UPDATE "+sq(table)+` SET "NativeArray"=`+sq(nativeArrays)+`(NULL,NULL,'NULL arrays') WHERE "Id"=2`)
	r, err := source.NewDaMengNative(sd, ss, source.ConnPoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dd, ds, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var saved RepairManifest
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 8192)}, Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: ds, CaptureRepair: true, ValidateData: true, PageSize: 1, RepairSink: func(p RepairManifest) error { saved = p; return nil }})
	// The only missing reference is buried inside an object -> matrix -> scalar
	// array. It must fail before even the target TYPE declarations are created.
	missing := fmt.Sprintf("DBGOLD_MISSING_%X", time.Now().UnixNano())
	exec(src, "INSERT INTO "+sq(table)+" VALUES(999,"+sq(outer)+"(NULL,NULL,NULL,"+sq(matrix)+"(NULL,"+sq(files)+"("+file+"))),NULL,NULL)", directory, missing)
	blocked := m.PreflightDamengNative(ctx)
	require.False(t, blocked.Ready)
	require.Contains(t, fmt.Sprint(blocked.Issues), "BFILE")
	var count int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.TYPE$='SCH' AND s.NAME=?`, ds).Scan(&count))
	require.Zero(t, count)
	exec(src, "DELETE FROM "+sq(table)+` WHERE "Id"=999`)
	frozen = m.PreflightDamengNative(ctx)
	require.True(t, frozen.Ready, "%+v", frozen.Issues)
	var refs []damengnative.Reference
	for _, o := range frozen.Objects {
		for _, ref := range o.External {
			if ref.Kind == "bfile" {
				refs = append(refs, ref)
			}
		}
	}
	require.Equal(t, []damengnative.Reference{{Kind: "bfile", Name: directory + ":" + filename}}, refs)
	report := m.Run(ctx)
	if report.HasFailures() {
		t.Logf("nested BFILE verification=%+v content=%+v logs=%v", report.NativeSchema.Verification, report.DataValidation, drainLogs(m.job))
		for _, side := range []struct {
			db *sql.DB
			q  string
		}{{src, sq(table)}, {dst, dq(table)}} {
			var value any
			if e := side.db.QueryRowContext(ctx, `SELECT "Object" FROM `+side.q+` WHERE "Id"=1`).Scan(&value); e == nil {
				t.Logf("native diagnostic %s: %v", side.q, nestedBFileFixtureSummary(nestedBFileFixtureValue(t, value)))
			}
		}
	}
	require.False(t, report.HasFailures())
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	b, err := json.Marshal(saved)
	require.NoError(t, err)
	var reloaded RepairManifest
	require.NoError(t, json.Unmarshal(b, &reloaded))
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reloaded.Objects))
	read := func(db *sql.DB, name string) []any {
		rows, e := db.QueryContext(ctx, `SELECT "Id","Object","Files","NativeArray" FROM `+name+` ORDER BY "Id"`)
		require.NoError(t, e)
		defer rows.Close()
		var out []any
		for rows.Next() {
			var id int
			var value, list, arrays any
			require.NoError(t, rows.Scan(&id, &value, &list, &arrays))
			out = append(out, []any{id, nestedBFileFixtureValue(t, value), nestedBFileFixtureValue(t, list), nestedBFileFixtureValue(t, arrays)})
		}
		require.NoError(t, rows.Err())
		return out
	}
	require.Equal(t, read(src, sq(table)), read(dst, dq(table)))
	// Change only a nested locator while preserving row count and primary key.
	// Content verification must find it, and report bounded fingerprints rather
	// than leaking the full object/LOB into a difference sample.
	exec(dst, "UPDATE "+dq(table)+` SET "Object"=`+dq(outer)+"(NULL,"+dq(files)+"(BFILENAME(?,?)),NULL,NULL) WHERE \"Id\"=3", directory, filename)
	result := validation.Validate(ctx, validation.NewSQLScanner(src, "dameng", ss), validation.NewSQLScanner(dst, "dameng", ds), validation.Plan{SourceTable: table, TargetTable: table})
	require.Equal(t, validation.StatusMismatch, result.Status)
	require.EqualValues(t, 1, result.ChangedRows)
	require.Equal(t, []string{"Object"}, result.Samples[0].Columns)
	exec(dst, "UPDATE "+dq(table)+` SET "Object"=`+dq(outer)+`(NULL,NULL,NULL,NULL) WHERE "Id"=3`)
	result = validation.Validate(ctx, validation.NewSQLScanner(src, "dameng", ss), validation.NewSQLScanner(dst, "dameng", ds), validation.Plan{SourceTable: table, TargetTable: table})
	require.Equal(t, validation.StatusPassed, result.Status)
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, m.cfg, reloaded.Objects))
}

func nestedBFileFixtureSummary(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = nestedBFileFixtureSummary(v[i])
		}
		return out
	case string:
		if len(v) > 120 {
			return fmt.Sprintf("[bytes=%d sha256=%x]", len(v), sha256.Sum256([]byte(v)))
		}
		return v
	default:
		return value
	}
}

func nestedBFileFixtureValue(t *testing.T, value any) any {
	t.Helper()
	switch v := value.(type) {
	case *dm.DmStruct:
		if v == nil || !v.Valid {
			return nil
		}
		values, err := v.GetAttributes()
		require.NoError(t, err)
		out := []any{"object"}
		for _, item := range values {
			out = append(out, nestedBFileFixtureValue(t, item))
		}
		return out
	case *dm.DmArray:
		if v == nil || !v.Valid {
			return nil
		}
		values, err := v.GetArray()
		require.NoError(t, err)
		out := []any{"array"}
		if values != nil {
			array := reflect.ValueOf(values)
			for i := 0; i < array.Len(); i++ {
				out = append(out, nestedBFileFixtureValue(t, array.Index(i).Interface()))
			}
		}
		return out
	case *dm.DmClob:
		if v == nil || !v.Valid {
			return nil
		}
		length, err := v.GetLength()
		require.NoError(t, err)
		text := ""
		if length > 0 {
			text, err = v.ReadString(1, int(length))
			require.NoError(t, err)
		}
		return []any{"clob", text}
	case *dm.DmBlob:
		if v == nil || !v.Valid {
			return nil
		}
		length, err := v.GetLength()
		require.NoError(t, err)
		data := make([]byte, int(length))
		if length > 0 {
			n, err := v.ReadAt(1, data)
			require.NoError(t, err)
			require.Equal(t, len(data), n)
		}
		return []any{"blob", data}
	case *dm.DmDecimal:
		if v == nil || !v.Valid {
			return nil
		}
		return []any{"decimal", v.String()}
	case *dm.DmIntervalDT:
		if v == nil || !v.Valid {
			return nil
		}
		return []any{"interval", v.String()}
	case time.Time:
		return []any{"time", v.Format(time.RFC3339Nano)}
	default:
		return value
	}
}
