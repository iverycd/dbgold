package source

import (
	"context"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"reflect"
	"strings"
	"testing"
)

func TestPGCatalogArraysPreserveOrder(t *testing.T) {
	for raw, want := range map[string][]int{"3 1 0": {3, 1, 0}, "{3,1}": {3, 1}, "[0:1]={3,1}": {3, 1}} {
		got, e := ParsePGNumbers(raw)
		if e != nil || !reflect.DeepEqual(got, want) {
			t.Fatal(raw, got, e)
		}
	}
	if _, e := ParsePGNumbers("{x}"); e == nil {
		t.Fatal("invalid array accepted")
	}
}
func TestPGMixedExpressionIndexIsNotSilentlyTruncated(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	r := &PostgresReader{db: db, schema: "Mixed", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT t.relname,i.relname`).WithArgs("Mixed").WillReturnRows(sqlmock.NewRows([]string{"table", "index", "keys", "unique", "primary", "method", "expr", "pred", "nkeys", "natts", "nulls", "valid", "immediate", "options", "def"}).AddRow("T", "Ix", "2 0 1", true, false, "btree", true, false, 3, 3, false, true, true, "0 0 0", "CREATE UNIQUE INDEX"))
	mock.ExpectQuery(`SELECT c.relname,a.attnum,a.attname`).WithArgs("Mixed").WillReturnRows(sqlmock.NewRows([]string{"t", "n", "a"}).AddRow("T", 1, "First").AddRow("T", 2, "Second"))
	indexes, e := r.pgIndexes(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(indexes) != 1 || !strings.Contains(indexes[0].Unsupported, "表达式") || !reflect.DeepEqual(indexes[0].Columns, []string{"Second", "First"}) {
		t.Fatal(indexes)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestPGColumnsWithoutNewCatalogFields(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	r := &PostgresReader{db: db, schema: "Mixed", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{}}}
	mock.ExpectQuery(`SELECT a.attnum,a.attname`).WithArgs("Mixed", "T").WillReturnRows(sqlmock.NewRows([]string{"num", "name", "type", "kind", "schema", "mod", "nullable", "default", "identity", "generated", "comment", "formatted"}).AddRow(1, "MixedID", "int4", "b", "pg_catalog", -1, false, `nextval('"Mixed"."Seq"'::regclass)`, "", "", nil, "integer"))
	info, e := r.pgColumns(context.Background(), "T")
	if e != nil || len(info.Columns) != 1 || info.Columns[0].Name != "MixedID" || info.Columns[0].Extra != "auto_increment" {
		t.Fatal(info, e)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}

func TestPGForeignKeysPairByOIDAndArrayPosition(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	r := &PostgresReader{db: db, schema: "Mixed", profile: PGProfile{Catalog: "pg_catalog"}}
	mock.ExpectQuery(`SELECT c.conname,t.relname`).WithArgs("Mixed").WillReturnRows(sqlmock.NewRows([]string{"n", "t", "rs", "rt", "keys", "refs", "del", "upd", "defer", "match", "oid"}).
		AddRow("SameName", "A", "OtherSchema", "Parent", "{2,1}", "{4,3}", "a", "c", false, "s", int64(42)).
		AddRow("SameName", "B", "Mixed", "Parent2", "{1}", "{1}", "a", "a", false, "s", int64(43)))
	mock.ExpectQuery(`SELECT c.relname,a.attnum,a.attname`).WithArgs("Mixed").WillReturnRows(sqlmock.NewRows([]string{"t", "n", "a"}).AddRow("A", 1, "One").AddRow("A", 2, "Two").AddRow("B", 1, "Only"))
	mock.ExpectQuery(`SELECT attnum,attname`).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"n", "a"}).AddRow(3, "X").AddRow(4, "Y"))
	mock.ExpectQuery(`SELECT attnum,attname`).WithArgs(int64(43)).WillReturnRows(sqlmock.NewRows([]string{"n", "a"}).AddRow(1, "ID"))
	fks, err := r.pgForeignKeys(context.Background())
	if err != nil || len(fks) != 2 {
		t.Fatal(fks, err)
	}
	if fks[0].RefSchema != "OtherSchema" || !reflect.DeepEqual(fks[0].Columns, []string{"Two", "One"}) || !reflect.DeepEqual(fks[0].RefColumns, []string{"Y", "X"}) || fks[1].RefColumns[0] != "ID" {
		t.Fatal(fks)
	}
	if e := mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestPGExtractionPermissionFailureIsNotEmptySuccess(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	r := &PostgresReader{db: db, schema: "public", profile: PGProfile{Catalog: "pg_catalog"}}
	mock.ExpectQuery(`SELECT t.relname,i.relname`).WillReturnError(fmt.Errorf("permission denied"))
	if indexes, err := r.pgIndexes(context.Background()); err == nil {
		t.Fatalf("empty success: %+v", indexes)
	}
	if e := mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}

func TestPGAutoIncrementRequiresExactNextvalDefault(t *testing.T) {
	for raw, want := range map[string]bool{`nextval('"Mixed"."Seq"'::regclass)`: true, `nextval('s'::regclass)+1`: false, `abs(nextval('s'::regclass))`: false, `nextval('s'::regclass)::text`: false} {
		if got := pgSimpleNextval(raw); got != want {
			t.Fatal(raw, got)
		}
	}
}
