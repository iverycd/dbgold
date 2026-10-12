package target

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMySQLDamengCopyDateBatch(t *testing.T) {
	w, mock := mockDM(t)
	w.srcType = "mysql"
	w.identCache["T"] = false
	mock.ExpectBegin()
	sql := `INSERT INTO "APP"."T" ("D", "AT", "TS", "TEXT", "BIT") VALUES (TO_DATE(?, 'YYYY-MM-DD'), TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF6'), TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF6'), ?, ?)`
	p := mock.ExpectPrepare(regexp.QuoteMeta(sql))
	p.ExpectExec().WithArgs("1900-01-01", "1900-01-01 00:00:00.000000", "2024-02-29 12:34:56.123456", "0000-00-00", int64(5)).WillReturnResult(sqlmock.NewResult(0, 1))
	p.ExpectExec().WithArgs("2024-02-29", nil, "1900-01-01 00:00:00.000000", "keep", int64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := w.CopyData(context.Background(), "T", []string{"D", "At", "Ts", "Text", "Bit"}, []string{"DATE", "DATETIME", "TIMESTAMP", "VARCHAR", "BIT"}, [][]interface{}{
		{"0000-00-00", "2024-00-01 12:34:56", "2024-02-29 12:34:56.123456", "0000-00-00", []byte{5}},
		{"2024-02-29", nil, "0000-00-00 00:00:00", "keep", []byte{0}},
	})
	require.NoError(t, err)
}

func TestMySQLDamengInvalidDateRollsBackBatch(t *testing.T) {
	w, mock := mockDM(t)
	w.srcType = "mysql"
	w.identCache["T"] = false
	mock.ExpectBegin()
	p := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO "APP"."T" ("AT") VALUES (TO_TIMESTAMP(?, 'YYYY-MM-DD HH24:MI:SS.FF6'))`))
	p.ExpectExec().WithArgs("1900-01-01 00:00:00.000000").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	err := w.CopyData(context.Background(), "T", []string{"At"}, []string{"DATETIME"}, [][]interface{}{{"0000-00-00 00:00:00"}, {"2024-02-30 12:00:00"}})
	require.ErrorContains(t, err, "T.At")
	require.ErrorContains(t, err, "无效 DATETIME 日期")
}
