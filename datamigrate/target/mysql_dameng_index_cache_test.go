package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func mysqlDMCacheIndexRows(values ...[]driver.Value) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "UNIQUENESS", "STATUS", "INDEX_TYPE"})
	for _, value := range values {
		rows.AddRow(value...)
	}
	return rows
}

func mysqlDMCacheColumnRows(values ...[]driver.Value) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"INDEX_OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "COLUMN_NAME", "COLUMN_POSITION", "DESCEND"})
	for _, value := range values {
		rows.AddRow(value...)
	}
	return rows
}

func expectMySQLDMCacheSnapshot(mock sqlmock.Sqlmock, schema string, indexes, columns *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesSQL)).WithArgs(schema).WillReturnRows(indexes).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexColumnsSQL)).WithArgs(schema).WillReturnRows(columns).RowsWillBeClosed()
}

func expectMySQLDMCacheExec(mock sqlmock.Sqlmock, w *DaMengWriter, idx source.IndexInfo, err error) {
	expected := mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx)))
	if err != nil {
		expected.WillReturnError(err)
	} else {
		expected.WillReturnResult(sqlmock.NewResult(0, 0))
	}
}

func TestMySQLDamengIndexCacheThousandPrimaryKeysNeedNoCatalog(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	const tables = 1000
	for i := 0; i < tables; i++ {
		pk := source.IndexInfo{TableName: fmt.Sprintf("Orders_%04d", i), IndexName: "PRIMARY", IsPrimary: true, Columns: []string{"Row_ID"}, Descending: []bool{true}}
		expectMySQLDMCacheExec(mock, w, pk, nil)
		require.NoError(t, w.CreateIndex(context.Background(), pk))
	}
	for i := 0; i < tables; i++ {
		idx := source.IndexInfo{TableName: fmt.Sprintf("Orders_%04d", i), IndexName: "ByRow", Columns: []string{"Row_ID"}}
		expectMySQLDMCacheExec(mock, w, idx, &dm.DmError{ErrCode: -3236})
		var events []repair.Event
		require.NoError(t, w.CreateIndex(observeMySQLDMIndex(&events, nil), idx))
		require.Equal(t, "reused", events[len(events)-1].Status)
	}
	// No catalog query is expected. Primary-key DDL always uses ASC, even if
	// the source metadata contains a descending flag.
}

func TestMySQLDamengIndexCacheThousandTablesShareOneUnorderedSnapshot(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	const tables = 1000
	headers, columns := mysqlDMCacheIndexRows(), mysqlDMCacheColumnRows()
	for i := tables - 1; i >= 0; i-- {
		table, name := fmt.Sprintf("ORDERS_%04d", i), fmt.Sprintf("EXISTING_%04d", i)
		headers.AddRow("APP", name, "APP", table, "UNIQUE", "VALID", "NORMAL")
		columns.AddRow("APP", name, "APP", table, "ID", 2, "ASC")
		columns.AddRow("APP", name, "APP", table, "CREATED", 1, "DESC")
	}
	for i := 0; i < tables; i++ {
		idx := source.IndexInfo{TableName: fmt.Sprintf("Orders_%04d", i), IndexName: "ByCreated", Columns: []string{"Created", "ID"}, Descending: []bool{true, false}}
		expectMySQLDMCacheExec(mock, w, idx, &dm.DmError{ErrCode: -3236})
		if i == 0 {
			expectMySQLDMCacheSnapshot(mock, "APP", headers, columns)
		}
		require.NoError(t, w.CreateIndex(context.Background(), idx))
	}
}

func TestMySQLDamengIndexCacheSuccessfulDDLDoesNotInventCoverage(t *testing.T) {
	for _, scenario := range []string{"failed primary key", "ordinary cannot cover unique", "different direction", "different order", "different table"} {
		t.Run(scenario, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			seed := source.IndexInfo{TableName: "Orders", IndexName: "Original", Columns: []string{"ID"}}
			requested := source.IndexInfo{TableName: "Orders", IndexName: "Requested", Columns: []string{"ID"}}
			var seedErr error
			switch scenario {
			case "failed primary key":
				seed.IsPrimary = true
				seedErr = errors.New("primary key creation failed")
			case "ordinary cannot cover unique":
				requested.IsUnique = true
			case "different direction":
				requested.Descending = []bool{true}
			case "different order":
				seed.Columns = []string{"ID", "Created"}
				requested.Columns = []string{"Created", "ID"}
			case "different table":
				seed.TableName = "OtherOrders"
			}
			expectMySQLDMCacheExec(mock, w, seed, seedErr)
			if seedErr != nil {
				require.ErrorIs(t, w.CreateIndex(context.Background(), seed), seedErr)
			} else {
				require.NoError(t, w.CreateIndex(context.Background(), seed))
			}
			original := &dm.DmError{ErrCode: -3236}
			expectMySQLDMCacheExec(mock, w, requested, original)
			expectMySQLDMCacheSnapshot(mock, "APP", mysqlDMCacheIndexRows(), mysqlDMCacheColumnRows())
			require.ErrorIs(t, w.CreateIndex(context.Background(), requested), original)
		})
	}
}

func TestMySQLDamengIndexCacheRejectsCatalogIdentityMixing(t *testing.T) {
	for _, scenario := range []string{"other schema", "other table", "column owner mismatch", "column table mismatch", "duplicate positions"} {
		t.Run(scenario, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			header := []driver.Value{"APP", "EXISTING", "APP", "ORDERS", "UNIQUE", "VALID", "NORMAL"}
			column := []driver.Value{"APP", "EXISTING", "APP", "ORDERS", "ID", 1, "ASC"}
			switch scenario {
			case "other schema":
				header[0], header[2], column[0], column[2] = "OTHER", "OTHER", "OTHER", "OTHER"
			case "other table":
				header[3], column[3] = "OTHER_ORDERS", "OTHER_ORDERS"
			case "column owner mismatch":
				column[0] = "OTHER"
			case "column table mismatch":
				column[3] = "OTHER_ORDERS"
			}
			columns := mysqlDMCacheColumnRows(column)
			if scenario == "duplicate positions" {
				columns.AddRow(column...)
			}
			idx := source.IndexInfo{TableName: "Orders", IndexName: "Requested", Columns: []string{"ID"}}
			original := &dm.DmError{ErrCode: -3236}
			expectMySQLDMCacheExec(mock, w, idx, original)
			expectMySQLDMCacheSnapshot(mock, "APP", mysqlDMCacheIndexRows(header), columns)
			require.ErrorIs(t, w.CreateIndex(context.Background(), idx), original)
		})
	}
}

func TestMySQLDamengIndexCacheIncludesDDLAfterSnapshot(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	first := source.IndexInfo{TableName: "ExistingOrders", IndexName: "Requested", Columns: []string{"ID"}}
	expectMySQLDMCacheExec(mock, w, first, &dm.DmError{ErrCode: -3236})
	expectMySQLDMCacheSnapshot(mock, "APP",
		mysqlDMCacheIndexRows([]driver.Value{"APP", "EXISTING", "APP", "EXISTINGORDERS", "NONUNIQUE", "VALID", "NORMAL"}),
		mysqlDMCacheColumnRows([]driver.Value{"APP", "EXISTING", "APP", "EXISTINGORDERS", "ID", 1, "ASC"}))
	require.NoError(t, w.CreateIndex(context.Background(), first))
	created := source.IndexInfo{TableName: "Orders", IndexName: "NewUnique", Columns: []string{"Created", "ID"}, IsUnique: true, Descending: []bool{true, false}}
	expectMySQLDMCacheExec(mock, w, created, nil)
	require.NoError(t, w.CreateIndex(context.Background(), created))
	requested := created
	requested.IndexName, requested.IsUnique = "AnotherName", false
	expectMySQLDMCacheExec(mock, w, requested, &dm.DmError{ErrCode: -3236})
	require.NoError(t, w.CreateIndex(context.Background(), requested))
}

func TestMySQLDamengIndexCacheFailedLoadsAreAtomicAndAttemptedOnce(t *testing.T) {
	for _, scenario := range []string{"headers query", "headers read", "headers scan", "columns query", "columns read", "columns scan"} {
		t.Run(scenario, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "Orders", IndexName: "Requested", Columns: []string{"ID"}}
			original := &dm.DmError{ErrCode: -3236}
			catalogErr := errors.New("catalog stream interrupted")
			expectMySQLDMCacheExec(mock, w, idx, original)
			headers := mysqlDMCacheIndexRows([]driver.Value{"APP", "EXISTING", "APP", "ORDERS", "UNIQUE", "VALID", "NORMAL"})
			headerQuery := mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesSQL)).WithArgs("APP")
			switch scenario {
			case "headers query":
				headerQuery.WillReturnError(catalogErr)
			case "headers read":
				headers.AddRow("APP", "LATER", "APP", "ORDERS", "UNIQUE", "VALID", "NORMAL").RowError(1, catalogErr)
				headerQuery.WillReturnRows(headers).RowsWillBeClosed()
			case "headers scan":
				headers.AddRow(nil, "LATER", "APP", "ORDERS", "UNIQUE", "VALID", "NORMAL")
				headerQuery.WillReturnRows(headers).RowsWillBeClosed()
			default:
				headerQuery.WillReturnRows(headers).RowsWillBeClosed()
				columnQuery := mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexColumnsSQL)).WithArgs("APP")
				columns := mysqlDMCacheColumnRows([]driver.Value{"APP", "EXISTING", "APP", "ORDERS", "ID", 1, "ASC"})
				switch scenario {
				case "columns query":
					columnQuery.WillReturnError(catalogErr)
				case "columns read":
					columns.AddRow("APP", "LATER", "APP", "ORDERS", "ID", 1, "ASC").RowError(1, catalogErr)
					columnQuery.WillReturnRows(columns).RowsWillBeClosed()
				case "columns scan":
					columns.AddRow("APP", "LATER", "APP", "ORDERS", "ID", "invalid-position", "ASC")
					columnQuery.WillReturnRows(columns).RowsWillBeClosed()
				}
			}
			var firstFailure error
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					idx.IndexName = "AnotherName"
					expectMySQLDMCacheExec(mock, w, idx, original)
				}
				err := w.CreateIndex(context.Background(), idx)
				require.ErrorIs(t, err, original)
				if attempt == 0 {
					firstFailure = err
				} else {
					require.EqualError(t, err, firstFailure.Error(), "reuse the recorded load failure without retrying metadata queries")
				}
				if !strings.HasSuffix(scenario, "scan") {
					require.ErrorIs(t, err, catalogErr)
				}
			}
			// Failed snapshots cannot authorize reuse from a partially read
			// matching row, and the second request must not query again.
		})
	}
}

func TestMySQLDamengIndexCacheSetSourceTypeStartsFreshRun(t *testing.T) {
	for _, changeSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("change_source=%v", changeSource), func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "Orders", IndexName: "Original", Columns: []string{"ID"}}
			expectMySQLDMCacheExec(mock, w, idx, nil)
			require.NoError(t, w.CreateIndex(context.Background(), idx))
			if changeSource {
				w.SetSourceType("oracle")
				idx.IndexName = "OracleAttempt"
				original := &dm.DmError{ErrCode: -3236}
				expectMySQLDMCacheExec(mock, w, idx, original)
				require.ErrorIs(t, w.CreateIndex(context.Background(), idx), original)
			}
			w.SetSourceType("mysql")
			idx.IndexName = "NextRun"
			original := &dm.DmError{ErrCode: -3236}
			expectMySQLDMCacheExec(mock, w, idx, original)
			expectMySQLDMCacheSnapshot(mock, "APP", mysqlDMCacheIndexRows(), mysqlDMCacheColumnRows())
			require.ErrorIs(t, w.CreateIndex(context.Background(), idx), original)
		})
	}
}

func TestMySQLDamengIndexCacheRunResetDiscardsCatalogAndLoadFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed_load=%v", failed), func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "Orders", IndexName: "FirstRun", Columns: []string{"ID"}}
			original := &dm.DmError{ErrCode: -3236}
			expectMySQLDMCacheExec(mock, w, idx, original)
			if failed {
				catalogErr := errors.New("catalog temporarily unavailable")
				mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesSQL)).WithArgs("APP").WillReturnError(catalogErr)
				require.ErrorIs(t, w.CreateIndex(context.Background(), idx), catalogErr)
			} else {
				expectMySQLDMCacheSnapshot(mock, "APP",
					mysqlDMCacheIndexRows([]driver.Value{"APP", "EXISTING", "APP", "ORDERS", "UNIQUE", "VALID", "NORMAL"}),
					mysqlDMCacheColumnRows([]driver.Value{"APP", "EXISTING", "APP", "ORDERS", "ID", 1, "ASC"}))
				require.NoError(t, w.CreateIndex(context.Background(), idx))
			}
			w.SetSourceType("mysql")
			idx.IndexName = "NextRun"
			expectMySQLDMCacheExec(mock, w, idx, original)
			expectMySQLDMCacheSnapshot(mock, "APP", mysqlDMCacheIndexRows(), mysqlDMCacheColumnRows())
			require.ErrorIs(t, w.CreateIndex(context.Background(), idx), original)
		})
	}
}

func TestMySQLDamengIndexCacheSchemaBucketsStayIsolated(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	idx := source.IndexInfo{TableName: "Orders", IndexName: "Original", Columns: []string{"ID"}}
	expectMySQLDMCacheExec(mock, w, idx, nil)
	require.NoError(t, w.CreateIndex(context.Background(), idx))
	w.schema = "OTHER"
	idx.IndexName = "Requested"
	original := &dm.DmError{ErrCode: -3236}
	catalogErr := errors.New("other schema catalog unavailable")
	expectMySQLDMCacheExec(mock, w, idx, original)
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesSQL)).WithArgs("OTHER").WillReturnError(catalogErr)
	err := w.CreateIndex(context.Background(), idx)
	require.ErrorIs(t, err, original, "APP's successful CREATE cannot cover OTHER")
	require.ErrorIs(t, err, catalogErr)
	w.schema = "APP"
	expectMySQLDMCacheExec(mock, w, idx, original)
	require.NoError(t, w.CreateIndex(context.Background(), idx), "OTHER's catalog failure must not invalidate APP's actual CREATE evidence")
	w.schema = "OTHER"
	expectMySQLDMCacheExec(mock, w, idx, original)
	require.ErrorIs(t, w.CreateIndex(context.Background(), idx), catalogErr, "the failed OTHER snapshot is remembered independently")
}

func TestMySQLDamengIndexCacheCreateTableInvalidatesBeforeFailedRebuild(t *testing.T) {
	for _, scenario := range []struct{ failed, catalog bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("failed_rebuild=%v/from_catalog=%v", scenario.failed, scenario.catalog), func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "Orders", IndexName: "Original", Columns: []string{"ID"}}
			if scenario.catalog {
				expectMySQLDMCacheExec(mock, w, idx, &dm.DmError{ErrCode: -3236})
				expectMySQLDMCacheSnapshot(mock, "APP",
					mysqlDMCacheIndexRows([]driver.Value{"APP", "EXISTING", "APP", "ORDERS", "UNIQUE", "VALID", "NORMAL"}),
					mysqlDMCacheColumnRows([]driver.Value{"APP", "EXISTING", "APP", "ORDERS", "ID", 1, "ASC"}))
			} else {
				expectMySQLDMCacheExec(mock, w, idx, nil)
			}
			require.NoError(t, w.CreateIndex(context.Background(), idx))
			ddl := []dialect.Statement{{SQL: `DROP TABLE "APP"."ORDERS" CASCADE CONSTRAINTS`}, {SQL: `CREATE TABLE "APP"."ORDERS" ("ID" INT)`}}
			mock.ExpectExec(regexp.QuoteMeta(ddl[0].SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
			create := mock.ExpectExec(regexp.QuoteMeta(ddl[1].SQL))
			var rebuildErr error
			if scenario.failed {
				rebuildErr = errors.New("table rebuild failed")
				create.WillReturnError(rebuildErr)
			} else {
				create.WillReturnResult(sqlmock.NewResult(0, 0))
			}
			err := w.CreateTable(context.Background(), ddl)
			if scenario.failed {
				require.ErrorIs(t, err, rebuildErr)
			} else {
				require.NoError(t, err)
			}
			idx.IndexName = "Requested"
			original := &dm.DmError{ErrCode: -3236}
			expectMySQLDMCacheExec(mock, w, idx, original)
			expectMySQLDMCacheSnapshot(mock, "APP", mysqlDMCacheIndexRows(), mysqlDMCacheColumnRows())
			require.ErrorIs(t, w.CreateIndex(context.Background(), idx), original)
		})
	}
}

func TestMySQLDamengIndexCacheRetainsExecutedDDLWhenTerminalPersistenceFails(t *testing.T) {
	for _, primary := range []bool{false, true} {
		t.Run(fmt.Sprintf("primary=%v", primary), func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "Orders", IndexName: "Original", Columns: []string{"ID"}, IsPrimary: primary}
			expectMySQLDMCacheExec(mock, w, idx, nil)
			persistErr := errors.New("terminal state save failed")
			var events []repair.Event
			err := w.CreateIndex(observeMySQLDMIndex(&events, func(event repair.Event) error {
				if event.Status == "succeeded" {
					return persistErr
				}
				return nil
			}), idx)
			require.ErrorIs(t, err, persistErr)
			requested := source.IndexInfo{TableName: "Orders", IndexName: "Requested", Columns: []string{"ID"}}
			expectMySQLDMCacheExec(mock, w, requested, &dm.DmError{ErrCode: -3236})
			require.NoError(t, w.CreateIndex(context.Background(), requested))
		})
	}
}
