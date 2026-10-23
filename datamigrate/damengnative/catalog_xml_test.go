package damengnative

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeXMLAttributeRestoresOnlyProvenColumnDatatypeTokens(t *testing.T) {
	ddl := `CREATE TABLE "Mixed"."T" ("Xml.Mixed" /* preserve TEXT */ TEXT DEFAULT XMLTYPE('<r attr="TEXT">XMLTYPE</r>'), "TEXT" CLOB, "Plain" TEXT DEFAULT 'TEXT', "CONSTRAINT" TEXT, CONSTRAINT "XMLTYPE" CHECK("Plain" <> 'TEXT')) /* TEXT remains */`
	wanted := map[string]bool{"Xml.Mixed": true, "TEXT": true, "CONSTRAINT": true}
	got, found, err := nativeXMLTableDDL(ddl, wanted)
	require.NoError(t, err)
	require.Equal(t, wanted, found)
	want := strings.Replace(ddl, `/* preserve TEXT */ TEXT`, `/* preserve TEXT */ XMLTYPE`, 1)
	want = strings.Replace(want, `"TEXT" CLOB`, `"TEXT" XMLTYPE`, 1)
	want = strings.Replace(want, `"CONSTRAINT" TEXT`, `"CONSTRAINT" XMLTYPE`, 1)
	require.Equal(t, want, got)
	buffer, err := RestoreNativeXMLTableDDL(ddl, []Column{{Name: "Xml.Mixed", DataType: "TEXT", Scale: 8}, {Name: "TEXT", DataType: "CLOB", Scale: 8}, {Name: "CONSTRAINT", DataType: "TEXT", Scale: 8}, {Name: "Plain", DataType: "TEXT"}})
	require.NoError(t, err)
	require.Equal(t, got, buffer, "physical partition and MV buffer definitions use the same exact column proof")
	again, _, err := nativeXMLTableDDL(got, wanted)
	require.NoError(t, err)
	require.Equal(t, got, again)
	for _, other := range []string{`CREATE INDEX "Mixed"."I" ON "Mixed"."T"("TEXT")`, `ALTER TABLE "Mixed"."T" ADD CHECK("TEXT" IS NOT NULL)`, `CREATE PROCEDURE "Mixed"."P" AS BEGIN PRINT 'CREATE TABLE T("TEXT" TEXT)'; END;`} {
		got, found, err := nativeXMLTableDDL(other, wanted)
		require.NoError(t, err)
		require.Empty(t, found)
		require.Equal(t, other, got)
	}
}

func TestNativeXMLAttributeRequiresExactColumnAndNativeScaleProof(t *testing.T) {
	for _, c := range []Column{{Name: "Ordinary", DataType: "TEXT"}, {Name: "OtherScale", DataType: "TEXT", Scale: 6}, {Name: "Decimal", DataType: "DECIMAL", Scale: 8}, {Name: "UDT", DataType: "CLOB", Scale: 8, TypeSchema: "Mixed"}} {
		require.False(t, nativeXMLColumn(c))
	}
	for _, kind := range []string{"TEXT", "CLOB"} {
		c := Column{Name: "X", DataType: kind, Scale: 8}
		require.True(t, nativeXMLColumn(c))
		require.Equal(t, "XMLTYPE", columnSQLType(c), "shared SQL type recovery also applies to MV history column plans")
	}
	object := Object{Units: []Unit{{SQL: `CREATE TABLE "Mixed"."T"("Other" TEXT)`, Phase: "before_data"}}}
	require.ErrorContains(t, restoreNativeXMLAttributes(&object, &Table{Name: "T", Columns: []Column{{Name: "Missing", DataType: "TEXT", Scale: 8}}}), "对应")
	_, err := RestoreNativeXMLTableDDL(`CREATE TABLE "Mixed"."T"("Other" TEXT)`, []Column{{Name: "Missing", DataType: "TEXT", Scale: 8}})
	require.ErrorContains(t, err, "对应")
	_, _, err = nativeXMLTableDDL(`CREATE TABLE T("X" VARCHAR(40))`, map[string]bool{"X": true})
	require.ErrorContains(t, err, "原生声明不同")
}

func TestNativeXMLCatalogKeepsRawValueCodecAndPreservesAttributeDuringVerification(t *testing.T) {
	for _, definitions := range []bool{false, true} {
		t.Run(map[bool]string{false: "inspection", true: "definition_and_final_verification"}[definitions], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			object := Object{ID: "table", Kind: "table", Name: "T", Schema: "Mixed"}
			if definitions {
				object.Units = []Unit{{SQL: `CREATE TABLE "Mixed"."T"("X" TEXT)`, Phase: "before_data"}}
			}
			r := &catalogReader{ctx: context.Background(), db: db, schema: "Mixed", definitions: definitions, verification: true, snapshot: &Snapshot{Objects: []Object{object}}, index: map[string]int{"table\x00T": 0}}
			mock.ExpectQuery(`SELECT c.COLUMN_NAME,c.DATA_TYPE`).WithArgs("Mixed", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "identity", "extra", "owner"}).AddRow("X", "TEXT", 0, 8, 0, 0, nil))
			if definitions {
				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Mixed"\."T"`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
				mock.ExpectQuery(`SELECT MAX\(DBMS_LOB.GETLENGTH\("X"\)\) FROM "Mixed"\."T"`).WillReturnRows(sqlmock.NewRows([]string{"length"}).AddRow(300013))
			}
			require.NoError(t, r.readTables())
			require.Empty(t, r.snapshot.Issues)
			column := r.snapshot.Tables[0].Columns[0]
			require.Equal(t, "TEXT", column.DataType, "the original driver text/CLOB codec is retained")
			require.EqualValues(t, 8, column.Scale)
			require.Equal(t, "XMLTYPE", column.SQLType)
			if definitions {
				require.Equal(t, `CREATE TABLE "Mixed"."T"("X" XMLTYPE)`, r.snapshot.Objects[0].Units[0].SQL)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
