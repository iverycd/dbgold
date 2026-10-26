package target

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func mysqlDMIndexFixture(t *testing.T) (*DaMengWriter, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return &DaMengWriter{db: db, schema: "APP", srcType: "mysql", dia: dialect.NewDaMeng()}, mock
}

func mysqlDMIndexDDL(w *DaMengWriter, idx source.IndexInfo) string {
	return w.dia.IndexStatements(w.schema, idx)[0].SQL
}

type mysqlDMIndexCatalogFixture struct {
	values   [][]driver.Value
	errorRow int
	err      error
}

func mysqlDMIndexCatalogRows(values ...[]driver.Value) *mysqlDMIndexCatalogFixture {
	return &mysqlDMIndexCatalogFixture{values: values}
}

func (f *mysqlDMIndexCatalogFixture) RowError(row int, err error) *mysqlDMIndexCatalogFixture {
	f.errorRow, f.err = row, err
	return f
}

// Preserve the compact joined fixture used by the coverage cases while
// returning the two schema-wide catalog result sets used by the writer.
func expectMySQLDMIndexCatalog(mock sqlmock.Sqlmock, schema, table string, fixture *mysqlDMIndexCatalogFixture) {
	headers := sqlmock.NewRows([]string{"OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "UNIQUENESS", "STATUS", "INDEX_TYPE"})
	columns := sqlmock.NewRows([]string{"INDEX_OWNER", "INDEX_NAME", "TABLE_OWNER", "TABLE_NAME", "COLUMN_NAME", "COLUMN_POSITION", "DESCEND"})
	seen := map[[2]driver.Value]bool{}
	for _, value := range fixture.values {
		key := [2]driver.Value{value[0], value[1]}
		if !seen[key] {
			headers.AddRow(value[0], value[1], schema, table, value[2], value[3], value[4])
			seen[key] = true
		}
		columns.AddRow(value[0], value[1], schema, table, value[5], value[6], value[7])
	}
	if fixture.err != nil {
		columns.RowError(fixture.errorRow, fixture.err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesSQL)).WithArgs(schema).WillReturnRows(headers).RowsWillBeClosed()
	mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexColumnsSQL)).WithArgs(schema).WillReturnRows(columns).RowsWillBeClosed()
}

func observeMySQLDMIndex(events *[]repair.Event, fail func(repair.Event) error) context.Context {
	return repair.WithObserver(context.Background(), func(event repair.Event) error {
		*events = append(*events, event)
		if fail != nil {
			return fail(event)
		}
		return nil
	})
}

func TestMySQLDamengIndexReusesVerifiedColumnList(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unique     bool
		uniqueness string
		kind       string
	}{
		{name: "primary key covers ordinary index", uniqueness: "UNIQUE", kind: "CLUSTER"},
		{name: "unique index covers ordinary index", uniqueness: "UNIQUE", kind: "NORMAL"},
		{name: "ordinary index covers ordinary index", uniqueness: "NONUNIQUE", kind: "NORMAL"},
		{name: "unique index covers unique index", unique: true, uniqueness: "UNIQUE", kind: "NORMAL"},
		{name: "primary key covers unique index", unique: true, uniqueness: "UNIQUE", kind: "CLUSTER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "Orders", IndexName: "RowId", Columns: []string{"Row_ID"}, IsUnique: tc.unique, Method: "BTREE"}
			ddl := mysqlDMIndexDDL(w, idx)
			mock.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnError(fmt.Errorf("wrapped: %w", &dm.DmError{ErrCode: -3236, ErrText: "此列列表已索引"}))
			expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows(
				[]driver.Value{"APP", "A_DIFFERENT_EXISTING_NAME", tc.uniqueness, "VALID", tc.kind, "ROW_ID", 1, "ASC"},
			))
			var events []repair.Event
			require.NoError(t, w.CreateIndex(observeMySQLDMIndex(&events, nil), idx))
			require.Len(t, events, 2)
			require.Equal(t, "running", events[0].Status)
			require.Equal(t, "reused", events[1].Status)
			for _, event := range events {
				require.Equal(t, ddl, event.SQL)
				require.Empty(t, event.Error)
			}
		})
	}
}

func TestMySQLDamengIndexCompositeOrderAndDirection(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	idx := source.IndexInfo{TableName: "Orders", IndexName: "ByDate", Columns: []string{"Created", "Id"}, Descending: []bool{true}}
	mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx))).WillReturnError(&dm.DmError{ErrCode: -3236})
	expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows(
		[]driver.Value{"APP", "WRONG_FIRST_CANDIDATE", "NONUNIQUE", "VALID", "NORMAL", "ID", 1, "ASC"},
		[]driver.Value{"APP", "RIGHT_CANDIDATE", "NONUNIQUE", "VALID", "NORMAL", "CREATED", 1, "DESC"},
		[]driver.Value{"APP", "RIGHT_CANDIDATE", "NONUNIQUE", "VALID", "NORMAL", "ID", 2, "ASC"},
	))
	require.NoError(t, w.CreateIndex(context.Background(), idx))
}

func TestMySQLDamengIndexRejectsUnverifiedCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unique bool
		rows   [][]driver.Value
	}{
		{name: "no index"},
		{name: "unique requires unique", unique: true, rows: [][]driver.Value{{"APP", "IX", "NONUNIQUE", "VALID", "NORMAL", "ID", 1, "ASC"}}},
		{name: "different column", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "OTHER", 1, "ASC"}}},
		{name: "different direction", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "ID", 1, "DESC"}}},
		{name: "invalid index", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "INVALID", "NORMAL", "ID", 1, "ASC"}}},
		{name: "expression index", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "FUNCTION-BASED NORMAL", "ID", 1, "ASC"}}},
		{name: "bitmap index", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "BITMAP", "ID", 1, "ASC"}}},
		{name: "expression column", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "NORMAL", nil, 1, "ASC"}}},
		{name: "unknown direction", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "ID", 1, nil}}},
		{name: "unknown uniqueness", rows: [][]driver.Value{{"APP", "IX", "UNKNOWN", "VALID", "NORMAL", "ID", 1, "ASC"}}},
		{name: "extra key column", rows: [][]driver.Value{
			{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "ID", 1, "ASC"},
			{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "OTHER", 2, "ASC"},
		}},
		{name: "missing key position", rows: [][]driver.Value{{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "ID", 2, "ASC"}}},
		{name: "separate indexes cannot form composite", rows: [][]driver.Value{
			{"APP", "IX_A", "UNIQUE", "VALID", "NORMAL", "ID", 1, "ASC"},
			{"APP", "IX_B", "UNIQUE", "VALID", "NORMAL", "OTHER", 1, "ASC"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "orders", IndexName: "ix", Columns: []string{"ID"}, IsUnique: tc.unique}
			if tc.name == "separate indexes cannot form composite" {
				idx.Columns = append(idx.Columns, "OTHER")
			}
			original := &dm.DmError{ErrCode: -3236, ErrText: "此列列表已索引"}
			mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx))).WillReturnError(original)
			expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows(tc.rows...))
			var events []repair.Event
			err := w.CreateIndex(observeMySQLDMIndex(&events, nil), idx)
			require.ErrorIs(t, err, original)
			require.ErrorContains(t, err, "核验已有达梦索引失败")
			require.Len(t, events, 2)
			require.Equal(t, "failed", events[1].Status)
			require.Contains(t, events[1].Error, "未找到")
		})
	}
}

func TestMySQLDamengIndexRejectsDifferentCompositeOrder(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	idx := source.IndexInfo{TableName: "orders", IndexName: "ix", Columns: []string{"A", "B"}}
	original := &dm.DmError{ErrCode: -3236}
	mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx))).WillReturnError(original)
	expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows(
		[]driver.Value{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "B", 1, "ASC"},
		[]driver.Value{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "A", 2, "ASC"},
	))
	require.ErrorIs(t, w.CreateIndex(context.Background(), idx), original)
}

func TestMySQLDamengIndexCatalogFailuresKeepOriginalError(t *testing.T) {
	for _, stage := range []string{"query", "read", "scan"} {
		t.Run(stage, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "orders", IndexName: "ix", Columns: []string{"ID"}}
			original := &dm.DmError{ErrCode: -3236}
			catalogErr := errors.New("catalog unavailable")
			mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx))).WillReturnError(original)
			switch stage {
			case "query":
				mock.ExpectQuery(regexp.QuoteMeta(mysqlDamengIndexesSQL)).WithArgs("APP").WillReturnError(catalogErr)
			case "read":
				expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows(
					[]driver.Value{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "ID", 1, "ASC"},
					[]driver.Value{"APP", "LATER", "UNIQUE", "VALID", "NORMAL", "ID", 1, "ASC"},
				).RowError(1, catalogErr))
			case "scan":
				expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows([]driver.Value{"APP", "IX", "UNIQUE", "VALID", "NORMAL", "ID", "not a position", "ASC"}))
			}
			err := w.CreateIndex(context.Background(), idx)
			require.ErrorIs(t, err, original)
			require.ErrorContains(t, err, "核验已有达梦索引失败")
			if stage != "scan" {
				require.ErrorIs(t, err, catalogErr)
			}
		})
	}
}

func TestMySQLDamengIndexCoverageIsLimitedToStructuredDuplicateError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  string
		primary bool
		err     error
		status  string
	}{
		{name: "new index succeeds", source: "mysql", status: "succeeded"},
		{name: "other database error", source: "mysql", err: &dm.DmError{ErrCode: -2007}, status: "failed"},
		{name: "textual error is insufficient", source: "mysql", err: errors.New("Error -3236: 此列列表已索引"), status: "failed"},
		{name: "primary key keeps original behavior", source: "mysql", primary: true, err: &dm.DmError{ErrCode: -3236}, status: "failed"},
		{name: "sqlserver source keeps original behavior", source: "sqlserver", err: &dm.DmError{ErrCode: -3236}, status: "failed"},
		{name: "oracle source keeps original behavior", source: "oracle", err: &dm.DmError{ErrCode: -3236}, status: "failed"},
		{name: "unknown source keeps original behavior", err: &dm.DmError{ErrCode: -3236}, status: "failed"},
		{name: "uncertain execution remains unknown", source: "mysql", err: context.DeadlineExceeded, status: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			w.srcType = tc.source
			idx := source.IndexInfo{TableName: "orders", IndexName: "ix", Columns: []string{"ID"}, IsPrimary: tc.primary}
			expected := mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx)))
			if tc.err == nil {
				expected.WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				expected.WillReturnError(tc.err)
			}
			var events []repair.Event
			err := w.CreateIndex(observeMySQLDMIndex(&events, nil), idx)
			if tc.err == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.err)
			}
			require.Len(t, events, 2)
			require.Equal(t, "running", events[0].Status)
			require.Equal(t, tc.status, events[1].Status)
		})
	}
}

func TestMySQLDamengUnsupportedIndexNeverExecutes(t *testing.T) {
	w, _ := mysqlDMIndexFixture(t)
	var events []repair.Event
	err := w.CreateIndex(observeMySQLDMIndex(&events, nil), source.IndexInfo{TableName: "orders", IndexName: "expression", Unsupported: "unsupported expression index"})
	require.ErrorContains(t, err, "unsupported expression index")
	require.Empty(t, events)
}

func TestMySQLDamengIndexObserverErrorsRemainFailures(t *testing.T) {
	for _, status := range []string{"running", "succeeded", "reused", "failed"} {
		t.Run(status, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			idx := source.IndexInfo{TableName: "orders", IndexName: "ix", Columns: []string{"ID"}}
			original := errors.New("permission denied")
			if status != "running" {
				expected := mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, idx)))
				switch status {
				case "succeeded":
					expected.WillReturnResult(sqlmock.NewResult(0, 0))
				case "reused":
					expected.WillReturnError(&dm.DmError{ErrCode: -3236})
					expectMySQLDMIndexCatalog(mock, "APP", "ORDERS", mysqlDMIndexCatalogRows(
						[]driver.Value{"APP", "PK", "UNIQUE", "VALID", "CLUSTER", "ID", 1, "ASC"},
					))
				case "failed":
					expected.WillReturnError(original)
				}
			}
			persistErr := errors.New("repair sink failed")
			var events []repair.Event
			err := w.CreateIndex(observeMySQLDMIndex(&events, func(event repair.Event) error {
				if event.Status == status {
					return persistErr
				}
				return nil
			}), idx)
			require.ErrorIs(t, err, persistErr)
			if status == "failed" {
				require.ErrorIs(t, err, original)
			}
			if status == "running" {
				require.Len(t, events, 1)
			} else {
				require.Len(t, events, 2)
			}
			require.Equal(t, status, events[len(events)-1].Status)
		})
	}
}
