package damengnative

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestBFileLocatorRetainsExactExternalIdentifiers(t *testing.T) {
	for _, tc := range []struct{ locator, directory, filename string }{
		{"Mixed.Dir:Keep 中文 'File'.bin", "Mixed.Dir", "Keep 中文 'File'.bin"},
		{" Leading Dir : trailing file ", " Leading Dir ", " trailing file "},
		{strings.Repeat("D", 128) + ":" + strings.Repeat("F", 256), strings.Repeat("D", 128), strings.Repeat("F", 256)},
	} {
		dir, file, err := ParseBFileLocator(tc.locator)
		require.NoError(t, err)
		require.Equal(t, tc.directory, dir)
		require.Equal(t, tc.filename, file)
	}
	for _, bad := range []string{"", "NoDelimiter", ":file", "Dir:", "Dir:File:Other", "Dir:\x00File", "Di\x00r:File", string([]byte{255}) + ":F", strings.Repeat("D", 129) + ":F", "D:" + strings.Repeat("F", 257)} {
		_, _, err := ParseBFileLocator(bad)
		require.Error(t, err)
	}
}

func TestBFileResourceProofIsReadOnlyAndPreservesIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exists  int
		failure error
	}{
		{name: "prepared", exists: 1},
		{name: "missing_file"},
		{name: "directory_permission_denied", failure: errors.New("无访问权限")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			query := mock.ExpectQuery(regexp.QuoteMeta(`SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`)).WithArgs(" Mixed.Dir ", "File 中文.bin")
			if tc.failure != nil {
				query.WillReturnError(tc.failure)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.exists))
			}
			err = CheckBFileResource(context.Background(), db, " Mixed.Dir :File 中文.bin")
			if tc.exists == 1 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if tc.failure != nil {
					require.ErrorIs(t, err, tc.failure)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet(), "resource proof cannot create directories or copy external files")
		})
	}
}

func TestBFileResourceRejectsInvalidLocatorBeforeQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	err = CheckBFileResource(context.Background(), db, "Dir:File:Other")
	require.ErrorContains(t, err, "定位器")
	require.NoError(t, mock.ExpectationsWereMet())
}
