package target

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const mysqlDamengFulltextDDL = `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`

func mysqlDamengFulltextIndex() source.IndexInfo {
	return source.IndexInfo{TableName: "articles", IndexName: "idx_body", Columns: []string{"body"}, Method: "FULLTEXT"}
}

func TestMySQLDamengFulltextExecutionIsObserved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{name: "success", status: "succeeded"},
		{name: "failure", err: &dm.DmError{ErrCode: -2007}, status: "failed"},
		{name: "duplicate is not B-tree coverage", err: &dm.DmError{ErrCode: -3236}, status: "failed"},
		{name: "timeout remains unknown", err: context.DeadlineExceeded, status: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := mysqlDMIndexFixture(t)
			expected := mock.ExpectExec(regexp.QuoteMeta(mysqlDamengFulltextDDL))
			if tc.err == nil {
				expected.WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				expected.WillReturnError(tc.err)
			}
			var events []repair.Event
			err := w.CreateIndex(observeMySQLDMIndex(&events, nil), mysqlDamengFulltextIndex())
			if tc.err == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.err)
			}
			require.Len(t, events, 2)
			require.Equal(t, "running", events[0].Status)
			require.Equal(t, tc.status, events[1].Status)
			for _, event := range events {
				require.Equal(t, mysqlDamengFulltextDDL, event.SQL)
			}
		})
	}
}

func TestMySQLDamengFulltextDoesNotProvideBTreeCoverage(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	fulltext := mysqlDamengFulltextIndex()
	mock.ExpectExec(regexp.QuoteMeta(mysqlDamengFulltextDDL)).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, w.CreateIndex(context.Background(), fulltext))

	ordinary := source.IndexInfo{TableName: fulltext.TableName, IndexName: "idx_body_btree", Columns: fulltext.Columns, Method: "BTREE"}
	original := &dm.DmError{ErrCode: -3236}
	mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, ordinary))).WillReturnError(original)
	expectMySQLDMIndexCatalog(mock, "APP", "ARTICLES", mysqlDMIndexCatalogRows())
	var events []repair.Event
	err := w.CreateIndex(observeMySQLDMIndex(&events, nil), ordinary)
	require.ErrorIs(t, err, original)
	require.ErrorContains(t, err, "未找到")
	require.Len(t, events, 2)
	require.Equal(t, "failed", events[1].Status)
}

func TestMySQLDamengFulltextDoesNotReuseBTreeCoverage(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	fulltext := mysqlDamengFulltextIndex()
	ordinary := source.IndexInfo{TableName: fulltext.TableName, IndexName: "idx_body_btree", Columns: fulltext.Columns, Method: "BTREE"}
	mock.ExpectExec(regexp.QuoteMeta(mysqlDMIndexDDL(w, ordinary))).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, w.CreateIndex(context.Background(), ordinary))

	original := &dm.DmError{ErrCode: -3236}
	mock.ExpectExec(regexp.QuoteMeta(mysqlDamengFulltextDDL)).WillReturnError(original)
	var events []repair.Event
	err := w.CreateIndex(observeMySQLDMIndex(&events, nil), fulltext)
	require.ErrorIs(t, err, original)
	require.Len(t, events, 2)
	require.Equal(t, "failed", events[1].Status)
}

func TestMySQLDamengFulltextPersistenceFailureIsReturned(t *testing.T) {
	w, mock := mysqlDMIndexFixture(t)
	mock.ExpectExec(regexp.QuoteMeta(mysqlDamengFulltextDDL)).WillReturnResult(sqlmock.NewResult(0, 0))
	persistErr := errors.New("observation unavailable")
	var events []repair.Event
	err := w.CreateIndex(observeMySQLDMIndex(&events, func(event repair.Event) error {
		if event.Status == "succeeded" {
			return persistErr
		}
		return nil
	}), mysqlDamengFulltextIndex())
	require.ErrorIs(t, err, persistErr)
	require.Len(t, events, 2)
	require.Equal(t, "succeeded", events[1].Status)
}

func TestInspectDamengFulltextDoesNotReadOrdinaryIndexCatalog(t *testing.T) {
	for _, method := range []string{"FULLTEXT", "fulltext"} {
		t.Run(method, func(t *testing.T) {
			w, _ := mysqlDMIndexFixture(t)
			idx := mysqlDamengFulltextIndex()
			idx.Method = method
			obj := repair.Object{
				Kind: "index", Schema: "APP", Table: "ARTICLES", TargetName: "ARTICLES_IDX_BODY", Index: &idx,
				Units: []repair.Unit{{SQL: mysqlDamengFulltextDDL, OriginalSQL: mysqlDamengFulltextDDL}},
			}
			v, err := InspectRepair(context.Background(), w, obj)
			require.NoError(t, err)
			require.Equal(t, "unknown", v.Status)
			require.Contains(t, v.Message, "全文索引")
			require.Contains(t, v.Message, "CONTEXT INDEX")
		})
	}
}
