package damengnative

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeBFilesFreezeExactDeduplicatedExternalReferences(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	r := &catalogReader{ctx: context.Background(), db: db, schema: "Src.Mixed"}
	object := Object{Kind: "table", Name: "Storage", Schema: r.schema}
	table := Table{Columns: []Column{{Name: "F\"1", DataType: "BFILE", Writable: true}, {Name: "F2", DataType: "BFILE", Writable: true}, {Name: "Text", DataType: "VARCHAR", Writable: true}, {Name: "Generated", DataType: "BFILE"}}}
	for _, column := range []string{"F\"1", "F2"} {
		q := Quote(column)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT " + q + ` FROM "Src.Mixed"."Actual Source" WHERE ` + q + " IS NOT NULL")).WillReturnRows(sqlmock.NewRows([]string{"locator"}).AddRow("Mixed.Dir:File 中文.bin").AddRow(nil).AddRow("Mixed.Dir:File 中文.bin")).RowsWillBeClosed()
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs("Mixed.Dir", "File 中文.bin").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
	require.NoError(t, r.freezeNativeBFiles(&object, table, "Actual Source"))
	require.Equal(t, []Reference{{Kind: "bfile", Name: "Mixed.Dir:File 中文.bin"}}, object.External)
	encoded, err := json.Marshal(object)
	require.NoError(t, err)
	var reloaded Object
	require.NoError(t, json.Unmarshal(encoded, &reloaded))
	require.Equal(t, object.External, reloaded.External, "the original locator is durable and outside schema mapping")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeBFilesRejectUnpreparedSourceResource(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "Src"}
	object := Object{Kind: "table", Name: "T"}
	table := Table{Columns: []Column{{Name: "F", DataType: "BFILE", Writable: true}}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "Src"."T" WHERE "F" IS NOT NULL`)).WillReturnRows(sqlmock.NewRows([]string{"locator"}).AddRow("Dir:Missing")).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs("Dir", "Missing").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(0))
	require.ErrorContains(t, r.freezeNativeBFiles(&object, table, "T"), "外部文件不可用")
	require.Empty(t, object.External)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeBFilesFreezeOnSourceObjectOnly(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		definitions, verification bool
	}{
		{name: "source_snapshot", definitions: true},
		{name: "target_inspection"},
		{name: "target_verification_preserves_unrelated_resources", definitions: true, verification: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			object := Object{ID: "table-id", Kind: "table", Name: "T", Schema: "Src", Units: []Unit{{SQL: `CREATE TABLE "Src"."T"("F" BFILE)`, Phase: "before_data"}}}
			r := &catalogReader{ctx: context.Background(), db: db, schema: "Src", definitions: tc.definitions, verification: tc.verification, snapshot: &Snapshot{Objects: []Object{object}}, index: map[string]int{"table\x00T": 0}}
			mock.ExpectQuery(`SELECT c.COLUMN_NAME,c.DATA_TYPE`).WithArgs("Src", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "precision", "scale", "identity", "extra", "owner"}).AddRow("F", "BFILE", 0, 6, 0, 0, nil))
			if tc.definitions {
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM "Src"."T"`)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
			}
			if tc.definitions && !tc.verification {
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "Src"."T" WHERE "F" IS NOT NULL`)).WillReturnRows(sqlmock.NewRows([]string{"locator"}).AddRow("Dir:File"))
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs("Dir", "File").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
			}
			require.NoError(t, r.readTables())
			require.Empty(t, r.snapshot.Issues)
			if tc.definitions && !tc.verification {
				require.Equal(t, []Reference{{Kind: "bfile", Name: "Dir:File"}}, r.snapshot.Objects[0].External)
			} else {
				require.Empty(t, r.snapshot.Objects[0].External)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestNativeBFilesFreezeStableOrderAcrossCursorOrders(t *testing.T) {
	for _, order := range [][]string{{"ZDir:last", "ADir:first", "ZDir:last", "MDir:middle"}, {"MDir:middle", "ZDir:last", "ADir:first"}} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		r := &catalogReader{ctx: context.Background(), db: db, schema: "Src"}
		object := Object{Kind: "table", Name: "T"}
		rows := sqlmock.NewRows([]string{"locator"})
		for _, locator := range order {
			rows.AddRow(locator)
		}
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "Src"."T" WHERE "F" IS NOT NULL`)).WillReturnRows(rows)
		for _, pair := range [][2]string{{"ADir", "first"}, {"MDir", "middle"}, {"ZDir", "last"}} {
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs(pair[0], pair[1]).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
		}
		require.NoError(t, r.freezeNativeBFiles(&object, Table{Columns: []Column{{Name: "F", DataType: "BFILE", Writable: true}}}, "T"))
		require.Equal(t, []Reference{{Kind: "bfile", Name: "ADir:first"}, {Kind: "bfile", Name: "MDir:middle"}, {Kind: "bfile", Name: "ZDir:last"}}, object.External)
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	}
}

func TestNativeBFilesPreserveCaseAndTrailingSpaceResourceIdentities(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "Src"}
	object := Object{Kind: "table", Name: "T"}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "F" FROM "Src"."T" WHERE "F" IS NOT NULL`)).WillReturnRows(sqlmock.NewRows([]string{"locator"}).AddRow("Dir:file ").AddRow("dir:File").AddRow("Dir:file").AddRow("Dir:File").AddRow("Dir:File"))
	var expected []Reference
	for _, locator := range []string{"Dir:File", "Dir:file", "Dir:file ", "dir:File"} {
		directory, filename, err := ParseBFileLocator(locator)
		require.NoError(t, err)
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs(directory, filename).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
		expected = append(expected, Reference{Kind: "bfile", Name: locator})
	}
	require.NoError(t, r.freezeNativeBFiles(&object, Table{Columns: []Column{{Name: "F", DataType: "BFILE", Writable: true}}}, "T"))
	require.Equal(t, expected, object.External, "OS resource identity must not inherit SQL collation or space normalization")
	require.NoError(t, mock.ExpectationsWereMet())
}
