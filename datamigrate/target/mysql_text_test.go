package target

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func TestMySQLTextDefaultsComeFromDatabase(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery(`SELECT DEFAULT_CHARACTER_SET_NAME,DEFAULT_COLLATION_NAME`).WithArgs("destination").WillReturnRows(sqlmock.NewRows([]string{"charset", "collation"}).AddRow("latin1", "latin1_swedish_ci"))
	mock.ExpectQuery(`SELECT c.CHARACTER_SET_NAME,s.MAXLEN`).WithArgs("latin1_swedish_ci").WillReturnRows(sqlmock.NewRows([]string{"charset", "maxlen"}).AddRow("latin1", 1))
	mock.ExpectQuery(`SELECT \(CONVERT`).WillReturnRows(sqlmock.NewRows([]string{"pad"}).AddRow(true))
	r, err := ReadMySQLDatabaseTextRules(context.Background(), db, "destination")
	if err != nil || r.Charset != "latin1" || r.Collation != "latin1_swedish_ci" || r.MaxBytes != 1 || !r.PadSpace {
		t.Fatal(r, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLTextRulesRejectUnsafeCatalogNames(t *testing.T) {
	for _, r := range []MySQLTextRules{{"utf8mb4;SELECT 1", "utf8mb4_bin", 4, true}, {"utf8mb4", "utf8mb4_bin --", 4, true}, {"utf8mb4", "utf8mb4_bin", 0, true}} {
		if err := r.Validate(); err == nil {
			t.Fatal(r)
		}
	}
}
