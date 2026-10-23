package target

import (
	"context"
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeBFileUsesOriginalDriverLocatorBinding(t *testing.T) {
	column := damengnative.Column{Name: "ExternalFile", DataType: "BFILE", SQLType: "BFILE", Writable: true}
	placeholder, err := nativePlaceholder(column)
	require.NoError(t, err)
	require.Equal(t, "?", placeholder, "a VARCHAR cast cannot preserve the native BFILE binding")
	for _, value := range []any{nil, "MixedDir:目录/文件.bin", []byte("MixedDir:目录/文件.bin")} {
		copied, size, err := nativeCopyValue(column, value, "Target")
		require.NoError(t, err)
		if value == nil {
			require.Nil(t, copied)
			require.Zero(t, size)
		} else {
			require.Equal(t, "MixedDir:目录/文件.bin", copied)
			require.Equal(t, len("MixedDir:目录/文件.bin"), size)
		}
	}
	for _, value := range []any{"missing_separator", "Dir:file:other", ":file", "Dir:", "Dir:a\x00b", []byte{0xff}, int64(42)} {
		_, _, err := nativeCopyValue(column, value, "Target")
		require.Error(t, err, "%v", value)
	}
}

func TestNativeBFileReferenceChecksPreparedResourceWithoutDDL(t *testing.T) {
	for _, exists := range []int{0, 1} {
		w, m := mockNativeDM(t)
		m.ExpectQuery(`SELECT DBMS_LOB.FILEEXISTS\(BFILENAME\(\?,\?\)\) FROM DUAL`).WithArgs("MixedDir", "ExactCase.bin").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(exists))
		err := w.CheckDamengNativeReference(context.Background(), damengnative.Reference{Kind: "bfile", Name: "MixedDir:ExactCase.bin"})
		if exists == 1 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
		require.NoError(t, m.ExpectationsWereMet())
	}
}
