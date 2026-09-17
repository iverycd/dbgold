package source

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPGSequenceMetadataNeverScansBusinessTable(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, called := range []bool{false, true} {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			r := &PostgresReader{db: db, schema: "Mixed", profile: PGProfile{Catalog: "pg_catalog", Fields: map[string]bool{"pg_sequence.seqincrement": modern}}}
			mock.ExpectQuery(`SELECT c.oid,n.nspname,c.relname`).WithArgs("Mixed").WillReturnRows(sqlmock.NewRows([]string{"oid", "schema", "name"}).AddRow(10, "Mixed", "Seq"))
			mock.ExpectQuery(`SELECT last_value,is_called FROM "Mixed"\."Seq"`).WillReturnRows(sqlmock.NewRows([]string{"last", "called"}).AddRow(100, called))
			if modern {
				mock.ExpectQuery(`SELECT seqincrement,seqmin,seqmax,seqcycle`).WithArgs(int64(10)).WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "cycle"}).AddRow(1, 1, 2147483647, false))
			} else {
				mock.ExpectQuery(`SELECT increment_by,min_value,max_value,is_cycled FROM "Mixed"\."Seq"`).WillReturnRows(sqlmock.NewRows([]string{"step", "min", "max", "cycle"}).AddRow(1, 1, 2147483647, false))
			}
			mock.ExpectQuery(`SELECT n.nspname,t.relname,a.attname`).WithArgs(int64(10)).WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column"}).AddRow("Mixed", "BusinessTable", "MixedID"))
			// No business-table query is permitted by this mock, including MAX/COUNT.
			snapshot := &PGSnapshot{Tables: []PGTable{{Info: TableDDLInfo{TableName: "BusinessTable"}}}}
			if err := r.loadPGSequences(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			seq := snapshot.Tables[0].Sequences[0]
			want := int64(100)
			if called {
				want++
			}
			if seq.StartValue != want || seq.TableName != "BusinessTable" || seq.ColumnName != "MixedID" {
				t.Fatal(seq)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}
	}
}
