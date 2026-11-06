package target

import (
	"context"
	"regexp"
	"testing"

	"dbgold/datamigrate/valueconv"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestKingbaseCopyDataSQLServerNumericBindings(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	w := &KingbaseWriter{db: db, schema: "App", valueConv: valueconv.NewPostgres()}
	w.SetSourceType("sqlserver")
	const decimal = "12345678901234567890.123456789012345678"
	const numeric = "-12345678901234567890123456789012345678"
	payload := []byte{0, 255, 0}
	rowVersion := []byte{0, 0, 0, 0, 0, 0, 0, 1}
	cols := []string{"Amount", "ExactInt", "Cash", "SmallCash", "Payload", "Fixed", "Image", "Version"}
	types := []string{"DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY", "VARBINARY", "BINARY", "IMAGE", "TIMESTAMP"}
	mock.ExpectBegin()
	stmt := mock.ExpectPrepare(regexp.QuoteMeta(`COPY "App"."Values" ("Amount", "ExactInt", "Cash", "SmallCash", "Payload", "Fixed", "Image", "Version") FROM STDIN`))
	// sqlmock checks both type and value: numeric parameters must be strings,
	// while the binary parameters must remain []byte for pq's bytea encoder.
	stmt.ExpectExec().WithArgs(decimal, numeric, "12.5000", "-3.4000", payload, []byte("0"), []byte{}, rowVersion).WillReturnResult(sqlmock.NewResult(0, 1))
	stmt.ExpectExec().WithArgs("0", "0", nil, nil, nil, nil, nil, nil).WillReturnResult(sqlmock.NewResult(0, 1))
	stmt.ExpectExec().WithArgs().WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	require.NoError(t, w.CopyData(context.Background(), "Values", cols, types, [][]any{
		{[]byte(decimal), []byte(numeric), []byte("12.5000"), []byte("-3.4000"), payload, []byte("0"), []byte{}, rowVersion},
		{[]byte("0"), "0", nil, nil, nil, nil, nil, nil},
	}))
}
