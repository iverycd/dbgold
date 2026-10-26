package damengnative

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// The real descriptor recursion is covered by the bundled driver's wire-codec
// roundtrips. This fixture verifies source cursor/resource orchestration through
// database/sql without a live database.
type fixtureBFileObject struct {
	locators []string
	err      error
}

func (value *fixtureBFileObject) VisitBFileLocators(visit func(string) error) error {
	for _, locator := range value.locators {
		if err := visit(locator); err != nil {
			return err
		}
	}
	return value.err
}

type nativeBFileFixtureConverter struct{}

func (nativeBFileFixtureConverter) ConvertValue(value any) (driver.Value, error) {
	if object, ok := value.(*fixtureBFileObject); ok {
		return object, nil
	}
	return driver.DefaultParameterConverter.ConvertValue(value)
}

func TestNativeBFilesComplexCursorsFreezeOnlyTypedLeaves(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(nativeBFileFixtureConverter{}))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	r := &catalogReader{ctx: context.Background(), db: db, schema: "Src.Mixed"}
	object := Object{Kind: "table", Name: "T", Schema: r.schema, External: []Reference{{Kind: "type", Schema: "External", Name: "Object"}}}
	table := Table{Columns: []Column{
		{Name: "Object", DataType: "CLASS1", TypeSchema: "External", TypeName: "Object", Writable: true},
		{Name: "Collection", DataType: "CLASS2", TypeSchema: "Src.Mixed", TypeName: "Files", Writable: true},
		{Name: "Generated", DataType: "CLASS1", TypeSchema: "External", TypeName: "Object"},
	}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Object" FROM "Src.Mixed"."T"`)).WillReturnRows(mock.NewRows([]string{"object"}).AddRow(&fixtureBFileObject{locators: []string{"ZDir:last", "ADir:first", "ZDir:last"}}).AddRow(nil)).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Collection" FROM "Src.Mixed"."T"`)).WillReturnRows(mock.NewRows([]string{"array"}).AddRow(&fixtureBFileObject{locators: []string{"ADir:first", "MiX.Dir:原名 "}})).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Generated" FROM "Src.Mixed"."T"`)).WillReturnRows(mock.NewRows([]string{"object"}).AddRow(&fixtureBFileObject{locators: []string{"ADir:first"}})).RowsWillBeClosed()
	for _, locator := range []string{"ADir:first", "MiX.Dir:原名 ", "ZDir:last"} {
		directory, filename, e := ParseBFileLocator(locator)
		require.NoError(t, e)
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs(directory, filename).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
	}
	require.NoError(t, r.freezeNativeBFiles(&object, table, "T"))
	require.Equal(t, []Reference{{Kind: "type", Schema: "External", Name: "Object"}, {Kind: "bfile", Name: "ADir:first"}, {Kind: "bfile", Name: "MiX.Dir:原名 "}, {Kind: "bfile", Name: "ZDir:last"}}, object.External)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNativeBFilesComplexFailureClosesCursorAndPublishesNoPartialReferences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   *fixtureBFileObject
		message string
	}{
		{"descriptor", &fixtureBFileObject{locators: []string{"Dir:valid"}, err: errors.New("incomplete descriptor")}, "incomplete descriptor"},
		{"invalid_leaf", &fixtureBFileObject{locators: []string{"Dir:valid", "not:a:file"}}, "BFILE 定位器"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(nativeBFileFixtureConverter{}))
			require.NoError(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			r := &catalogReader{ctx: context.Background(), db: db, schema: "Src"}
			object := Object{Kind: "table", Name: "T"}
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT "Object" FROM "Src"."T"`)).WillReturnRows(mock.NewRows([]string{"object"}).AddRow(tc.value)).RowsWillBeClosed()
			err = r.freezeNativeBFiles(&object, Table{Columns: []Column{{Name: "Object", DataType: "CLASS1", TypeSchema: "Src", TypeName: "Object", Writable: true}}}, "T")
			require.ErrorContains(t, err, tc.message)
			require.Empty(t, object.External)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
