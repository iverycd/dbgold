package target

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	goora "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"
)

func oracleDMLoadWriter(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	w, mock := oracleDamengBindingWriter(t)
	w.oracleDM.load = nil
	return w, mock
}

func expectOracleDMLoadColumn(mock sqlmock.Sqlmock, kind string, chars, bytes int64, units string, charset int) {
	mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadColumnsSQL)).WithArgs("APP", "T").
		WillReturnRows(sqlmock.NewRows([]string{"name", "type", "chars", "bytes", "units"}).AddRow("V", kind, chars, bytes, units)).RowsWillBeClosed()
	if charset >= 0 {
		mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadCharsetSQL)).WillReturnRows(sqlmock.NewRows([]string{"charset"}).AddRow(charset))
	}
}

func expectOracleDMLoadInsert(mock sqlmock.Sqlmock, value any) {
	mock.ExpectBegin()
	stmt := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("V") VALUES (?)`))
	stmt.ExpectExec().WithArgs(value).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func TestOracleDMLoadActualNationalCapacityAndCache(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	expectOracleDMLoadColumn(mock, "NVARCHAR", 36, 144, "C", 1)
	// Every character is Chinese: the actual 36 CHAR column can hold this,
	// independently of CAST(... AS NVARCHAR(2 CHAR)) behavior.
	value := strings.Repeat("汉", 36)
	for i := 0; i < 2; i++ {
		expectOracleDMLoadInsert(mock, value)
		require.NoError(t, w.CopyData(context.Background(), "T", []string{"V"}, []string{"VARCHAR2"}, [][]any{{value}}))
	}
}

func TestOracleDMLoadRejectsWholePageBeforeTransaction(t *testing.T) {
	for _, tc := range []struct {
		name, kind, units, sourceType, message string
		chars, bytes                           int64
		charset                                int
		bad                                    any
	}{
		{name: "character_count_including_spaces", kind: "NVARCHAR", units: "C", chars: 36, bytes: 144, charset: 1, sourceType: "VARCHAR2", bad: strings.Repeat("A", 36) + " ", message: "37 个字符超过目标列 36"},
		{name: "utf8_byte_limit", kind: "VARCHAR", units: "B", chars: 36, bytes: 36, charset: 1, sourceType: "VARCHAR2", bad: strings.Repeat("汉", 13), message: "39 字节超过目标列 36"},
		{name: "character_column_physical_limit", kind: "NCHAR", units: "C", chars: 2, bytes: 4, charset: 1, sourceType: "NCHAR", bad: "汉汉", message: "6 字节超过目标列 4"},
		{name: "invalid_utf8", kind: "NVARCHAR", units: "C", chars: 36, bytes: 144, charset: 1, sourceType: "NVARCHAR2", bad: string([]byte{0xff}), message: "无效 UTF-8"},
		{name: "unrepresentable_euckr", kind: "NVARCHAR", units: "C", chars: 36, bytes: 144, charset: 2, sourceType: "NVARCHAR2", bad: "😀", message: "无损编码"},
		{name: "binary_bytes", kind: "VARBINARY", units: "B", bytes: 2, charset: -1, sourceType: "RAW", bad: []byte{1, 2, 3}, message: "3 字节超过目标列 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := oracleDMLoadWriter(t)
			expectOracleDMLoadColumn(mock, tc.kind, tc.chars, tc.bytes, tc.units, tc.charset)
			var good any = "A"
			if tc.sourceType == "RAW" {
				good = []byte{1}
			}
			err := w.CopyData(context.Background(), "T", []string{"V"}, []string{tc.sourceType}, [][]any{{good}, {tc.bad}})
			require.ErrorContains(t, err, "T.V 第 2 行")
			require.ErrorContains(t, err, tc.message)
		})
	}
}

func TestOracleDMLoadUsesTargetEncodingInsteadOfOracleByteLength(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	expectOracleDMLoadColumn(mock, "VARCHAR", 3, 3, "B", 0)
	expectOracleDMLoadInsert(mock, "汉A") // GB18030: 2 + 1 bytes; UTF-8 would be 4.
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"V"}, []string{"VARCHAR2"}, [][]any{{"汉A"}}))
}

func TestOracleDMLoadPreservesInlineNULAndNull(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	expectOracleDMLoadColumn(mock, "NVARCHAR", 3, 12, "C", 1)
	for _, value := range []any{"汉\x00A", nil} {
		expectOracleDMLoadInsert(mock, value)
		require.NoError(t, w.CopyData(context.Background(), "T", []string{"V"}, []string{"NVARCHAR2"}, [][]any{{value}}))
	}
}

func TestOracleDMLoadKeepsNULAndDoesNotLimitLOBAsVarchar(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	value := strings.Repeat("汉", 3000) + "\x00"
	mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadCharsetSQL)).WillReturnRows(sqlmock.NewRows([]string{"charset"}).AddRow(1))
	expectOracleDMLoadInsert(mock, oracleDamengClobArgument(value))
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"V"}, []string{"CLOB"}, [][]any{{value}}))
}

func TestOracleDMLoadBFileLocatorIsNotLimitedByStorageDescriptorSize(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	w.oracleDM.load = &oracleDMLoadCache{tables: map[string]map[string]oracleDMLoadColumn{"T": {"V": {kind: "BFILE", bytes: 16}}}}
	dir, file := "MixedDirectory", "source-file-with-a-long-name.txt"
	mock.ExpectExec(regexp.QuoteMeta("SELECT :1 FROM DUAL")).WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	locator, err := goora.CreateBFile(w.db, dir, file)
	require.NoError(t, err)
	mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadCharsetSQL)).WillReturnRows(sqlmock.NewRows([]string{"charset"}).AddRow(1))
	expectOracleDMLoadInsert(mock, dir+":"+file)
	require.NoError(t, w.CopyData(context.Background(), "T", []string{"V"}, []string{"BFILE"}, [][]any{{locator}}))
}

func TestOracleDMLoadRollsBackPageOnActualInsertFailure(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	expectOracleDMLoadColumn(mock, "NVARCHAR", 36, 144, "C", 1)
	mock.ExpectBegin()
	stmt := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT /*+ MY_STRICT_TABLES(7) */ INTO "APP"."T" ("V") VALUES (?)`))
	stmt.ExpectExec().WithArgs("汉A").WillReturnResult(sqlmock.NewResult(0, 1))
	stmt.ExpectExec().WithArgs("汉B").WillReturnError(errors.New("write failed"))
	mock.ExpectRollback()
	err := w.CopyData(context.Background(), "T", []string{"V"}, []string{"NVARCHAR2"}, [][]any{{"汉A"}, {"汉B"}})
	require.ErrorContains(t, err, "write failed")
}

func TestOracleDMLoadMetadataErrorsPreserveCauseAndRetry(t *testing.T) {
	w, mock := oracleDMLoadWriter(t)
	mock.ExpectQuery(regexp.QuoteMeta(oracleDMLoadColumnsSQL)).WithArgs("APP", "T").WillReturnError(sql.ErrConnDone)
	_, err := w.prepareOracleDMLoadColumns(context.Background(), "T", []string{"V"}, []string{"VARCHAR2"})
	require.ErrorIs(t, err, sql.ErrConnDone)
	expectOracleDMLoadColumn(mock, "NVARCHAR", 36, 144, "C", 1)
	_, err = w.prepareOracleDMLoadColumns(context.Background(), "T", []string{"V"}, []string{"VARCHAR2"})
	require.NoError(t, err)
	w.resetOracleDMLoadColumns()
	expectOracleDMLoadColumn(mock, "VARCHAR", 20, 20, "B", -1)
	cols, err := w.prepareOracleDMLoadColumns(context.Background(), "T", []string{"V"}, []string{"VARCHAR2"})
	require.NoError(t, err)
	require.Equal(t, int64(20), cols[0].bytes)
}
