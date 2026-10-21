package source

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestNormalizeMySQLValues(t *testing.T) {
	values := []interface{}{[]byte("text\x00value"), []byte{0, 1}, []byte{2}, int64(3)}
	normalizeMySQLValues(values, []string{"VARCHAR", "BLOB", "BIT", "BIGINT"})
	if values[0] != "textvalue" {
		t.Fatalf("text value=%q", values[0])
	}
	if _, ok := values[1].([]byte); !ok {
		t.Fatal("BLOB must remain []byte")
	}
	if _, ok := values[2].([]byte); !ok {
		t.Fatal("BIT must remain []byte")
	}
}

func TestMySQLTableInfoPreservesTemporalPrecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "DATETIME_PRECISION"}).
		AddRow("OccurredAt", "datetime", nil, nil, nil, "NO", nil, "", 6).
		AddRow("UpdatedAt", "timestamp", nil, nil, nil, "YES", "CURRENT_TIMESTAMP", "DEFAULT_GENERATED", 0).
		AddRow("Label", "varchar", 80, nil, nil, "YES", nil, "", nil)
	mock.ExpectQuery(`SELECT COLUMN_NAME, DATA_TYPE, CHARACTER_MAXIMUM_LENGTH, NUMERIC_PRECISION, NUMERIC_SCALE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA, DATETIME_PRECISION`).WithArgs("SourceDB", "MixedTable").WillReturnRows(rows).RowsWillBeClosed()
	r := &MySQLReader{db: db, dbName: "SourceDB"}
	got, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
	if err != nil {
		t.Fatal(err)
	}
	if got.TableName != "MixedTable" || len(got.Columns) != 3 || got.Columns[0].Name != "OccurredAt" || got.Columns[0].DateTimePrecision != 6 || got.Columns[1].DateTimePrecision != 0 || got.Columns[2].DateTimePrecision != 0 {
		t.Fatalf("metadata: %+v", got)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
