package source

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func triggerRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TRIGGER_NAME", "EVENT_OBJECT_TABLE", "TRIGGER_SCHEMA", "ACTION_TIMING", "EVENT_MANIPULATION", "ACTION_STATEMENT", "SQL_MODE", "DEFINER", "ACTION_ORDER"})
}

func TestMySQLGetTriggersPreservesOriginalNamesAndDDL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body := "BEGIN\n SET NEW.`MixedName` = 'Do NOT Lower';\nEND"
	ddl := "CREATE DEFINER=`MixedUser`@`localhost` TRIGGER `SourceDB`.`MixedTrigger` BEFORE UPDATE ON `MixedTable` FOR EACH ROW " + body
	mock.ExpectQuery(`SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, TRIGGER_SCHEMA,`).WithArgs("SourceDB").WillReturnRows(triggerRows().AddRow("MixedTrigger", "MixedTable", "SourceDB", "BEFORE", "UPDATE", body, "STRICT_TRANS_TABLES", "MixedUser@localhost", 2)).RowsWillBeClosed()
	// Different MySQL versions expose different column lists; locate by name.
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TRIGGER `SourceDB`.`MixedTrigger`")).WillReturnRows(sqlmock.NewRows([]string{"Trigger", "sql_mode", "character_set_client", "SQL Original Statement", "Created"}).AddRow("MixedTrigger", "STRICT_TRANS_TABLES", "utf8mb4", ddl, nil)).RowsWillBeClosed()
	r := &MySQLReader{db: db, dbName: "SourceDB"}
	got, err := r.GetTriggers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("triggers %v", got)
	}
	g := got[0]
	if g.Name != "MixedTrigger" || g.TableName != "MixedTable" || g.Schema != "SourceDB" || g.Body != body || g.SourceDDL != ddl || g.Definer != "MixedUser@localhost" || g.ActionOrder != 2 || g.MetadataError != "" {
		t.Fatalf("metadata changed: %+v", g)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLGetTriggersReportsObjectMetadataFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       any
		queryError bool
		ddlCols    []string
		ddl        []any
	}{
		{name: "show permission", body: "SET NEW.x=1", queryError: true},
		{name: "missing DDL column", body: "SET NEW.x=1", ddlCols: []string{"Trigger"}, ddl: []any{"T"}},
		{name: "null body", body: nil, ddlCols: []string{"SQL Original Statement"}, ddl: []any{"CREATE TRIGGER T BEFORE INSERT ON A FOR EACH ROW SET NEW.x=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectQuery(`SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, TRIGGER_SCHEMA,`).WithArgs("S").WillReturnRows(triggerRows().AddRow("T", "A", "S", "BEFORE", "INSERT", tc.body, "", "root@localhost", 1))
			q := mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TRIGGER `S`.`T`"))
			if tc.queryError {
				q.WillReturnError(fmt.Errorf("TRIGGER command denied"))
			} else {
				rows := sqlmock.NewRows(tc.ddlCols)
				switch len(tc.ddl) {
				case 1:
					rows.AddRow(tc.ddl[0])
				}
				q.WillReturnRows(rows)
			}
			r := &MySQLReader{db: db, dbName: "S"}
			got, err := r.GetTriggers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].MetadataError == "" {
				t.Fatalf("metadata failure was hidden: %+v", got)
			}
			if tc.body != nil && got[0].Body != tc.body {
				t.Fatal("original body lost")
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLTriggerDDLQuotesIdentifiersAndFindsCreateColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TRIGGER `S``DB`.`T``Name`")).WillReturnRows(sqlmock.NewRows([]string{"Trigger", "Create Trigger", "sql_mode"}).AddRow("T`Name", "CREATE TRIGGER original", ""))
	r := &MySQLReader{db: db}
	ddl, err := r.readTriggerDDL(context.Background(), "S`DB", "T`Name")
	if err != nil {
		t.Fatal(err)
	}
	if ddl != "CREATE TRIGGER original" {
		t.Fatal(ddl)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLGetTriggersMetadataQueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, TRIGGER_SCHEMA,`).WithArgs("S").WillReturnError(fmt.Errorf("metadata unavailable"))
	r := &MySQLReader{db: db, dbName: "S"}
	_, err = r.GetTriggers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "metadata unavailable") {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
