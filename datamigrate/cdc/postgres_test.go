package cdc

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newSequenceMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
		db.Close()
	})
	return db, mock
}

func TestSequenceCandidateHonorsNameCase(t *testing.T) {
	table := &TableInfo{Name: "Code_ItemLev", Columns: []string{"SYSID"}}

	lower := &PostgresApplier{lower: true}
	if candidate, column := lower.sequenceCandidate(table, 0); candidate != "seq_code_itemlev_sysid" || column != "sysid" {
		t.Fatalf("lower-case candidate mismatch: candidate=%s column=%s", candidate, column)
	}

	preserve := &PostgresApplier{lower: false}
	if candidate, column := preserve.sequenceCandidate(table, 0); candidate != "seq_Code_ItemLev_SYSID" || column != "SYSID" {
		t.Fatalf("original-case candidate mismatch: candidate=%s column=%s", candidate, column)
	}
	if got := qualifiedIdent("TargetSchema", "seq_Code_ItemLev_SYSID"); got != `"TargetSchema"."seq_Code_ItemLev_SYSID"` {
		t.Fatalf("qualified candidate mismatch: %s", got)
	}
}

func TestResolveSequenceUsesOwnedSequence(t *testing.T) {
	db, mock := newSequenceMock(t)
	a := &PostgresApplier{db: db, schema: "TargetSchema", lower: true}
	table := &TableInfo{Name: "Code_ItemLev", Columns: []string{"SYSID"}}

	mock.ExpectQuery(`SELECT pg_get_serial_sequence($1,$2)`).
		WithArgs(`"TargetSchema"."code_itemlev"`, "sysid").
		WillReturnRows(sqlmock.NewRows([]string{"pg_get_serial_sequence"}).AddRow(`"TargetSchema"."owned_sequence"`))

	got, err := a.resolveSequence(context.Background(), table, 0)
	if err != nil {
		t.Fatalf("resolve owned sequence: %v", err)
	}
	if got != `"TargetSchema"."owned_sequence"` {
		t.Fatalf("owned sequence mismatch: %s", got)
	}
}

func TestSyncSequencesFallsBackToCatalog(t *testing.T) {
	db, mock := newSequenceMock(t)
	a := &PostgresApplier{db: db, schema: "target", lower: true}
	tables := []TableInfo{{Name: "code_itemlev", Columns: []string{"SYSID"}, AutoIncrement: []int{0}}}

	mock.ExpectQuery(`SELECT pg_get_serial_sequence($1,$2)`).
		WithArgs(`"target"."code_itemlev"`, "sysid").
		WillReturnRows(sqlmock.NewRows([]string{"pg_get_serial_sequence"}).AddRow(nil))
	mock.ExpectQuery(sequenceCatalogQuery).
		WithArgs("target", "seq_code_itemlev_sysid").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`SELECT setval($1::regclass, COALESCE((SELECT MAX("sysid") FROM "target"."code_itemlev"), 0) + 1, false)`).
		WithArgs(`"target"."seq_code_itemlev_sysid"`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := a.SyncSequences(context.Background(), tables); err != nil {
		t.Fatalf("sync sequence through catalog fallback: %v", err)
	}
}

func TestResolveSequenceReportsMissingCandidate(t *testing.T) {
	db, mock := newSequenceMock(t)
	a := &PostgresApplier{db: db, schema: "target", lower: true}
	table := &TableInfo{Name: "code_itemlev", Columns: []string{"SYSID"}}

	mock.ExpectQuery(`SELECT pg_get_serial_sequence($1,$2)`).
		WithArgs(`"target"."code_itemlev"`, "sysid").
		WillReturnRows(sqlmock.NewRows([]string{"pg_get_serial_sequence"}).AddRow(nil))
	mock.ExpectQuery(sequenceCatalogQuery).
		WithArgs("target", "seq_code_itemlev_sysid").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	_, err := a.resolveSequence(context.Background(), table, 0)
	if err == nil || !strings.Contains(err.Error(), `候选序列="target"."seq_code_itemlev_sysid"`) {
		t.Fatalf("expected missing candidate detail, got: %v", err)
	}
}

func TestResolveSequencePreservesCatalogError(t *testing.T) {
	db, mock := newSequenceMock(t)
	a := &PostgresApplier{db: db, schema: "target", lower: true}
	table := &TableInfo{Name: "code_itemlev", Columns: []string{"SYSID"}}
	catalogErr := errors.New("catalog permission denied")

	mock.ExpectQuery(`SELECT pg_get_serial_sequence($1,$2)`).
		WithArgs(`"target"."code_itemlev"`, "sysid").
		WillReturnError(errors.New("function does not exist"))
	mock.ExpectQuery(sequenceCatalogQuery).
		WithArgs("target", "seq_code_itemlev_sysid").
		WillReturnError(catalogErr)

	_, err := a.resolveSequence(context.Background(), table, 0)
	if !errors.Is(err, catalogErr) || !strings.Contains(err.Error(), "function does not exist") {
		t.Fatalf("expected both lookup errors, got: %v", err)
	}
}
