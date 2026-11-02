package damengnative

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestMaterializedHistoryPlanFailureIsScopedToKnownFrozenGroup(t *testing.T) {
	r := &catalogReader{snapshot: &Snapshot{Objects: []Object{
		{ID: "base", Kind: "table", Name: "Base"},
		{ID: "independent", Kind: "table", Name: "Independent"},
		{ID: "carrier", Kind: "table", Name: "Store"},
		{ID: "mv", Kind: "materialized_view", Name: "Mv", Table: "Store", Materialized: &MaterializedState{Baseline: true}},
		{ID: "sibling", Kind: "materialized_view", Name: "Sibling", Materialized: &MaterializedState{Baseline: true}},
		{ID: "log", Kind: "materialized_view_log", Name: "Log", Table: "Base"},
	}}}
	before := len(r.snapshot.Objects)
	r.snapshot.Objects = append(r.snapshot.Objects, Object{ID: "new-helper", Kind: "table", Name: "HistoryBuffer", Generated: true})
	r.recordMaterializedHistoryFailure(errors.New("storage definition unavailable"), before)
	require.Nil(t, r.taskError)
	ids := []string{}
	for _, issue := range r.snapshot.Issues {
		require.NotEmpty(t, issue.ObjectID)
		require.Contains(t, issue.Reason, "storage definition unavailable")
		ids = append(ids, issue.ObjectID)
	}
	require.ElementsMatch(t, []string{"carrier", "mv", "sibling", "log", "new-helper"}, ids)
}

func TestMaterializedHistoryConnectionFailureRemainsTaskScoped(t *testing.T) {
	cause := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection failed")}
	r := &catalogReader{snapshot: &Snapshot{Objects: []Object{{ID: "mv", Kind: "materialized_view", Materialized: &MaterializedState{Baseline: true}}}}}
	r.recordMaterializedHistoryFailure(cause, 1)
	require.ErrorIs(t, r.taskError, cause)
	require.Len(t, r.snapshot.Issues, 1)
	require.Empty(t, r.snapshot.Issues[0].ObjectID)
}

func TestNativeTableConnectionFailureDoesNotRetryDictionaryFallback(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := nativeMVReader(db)
	r.ctx = context.Background()
	cause := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("session lost")}
	mock.ExpectQuery(`SELECT c.COLUMN_NAME,c.DATA_TYPE`).WithArgs("App", "Storage").WillReturnError(cause)
	require.ErrorIs(t, r.readTables(), cause)
	require.NoError(t, mock.ExpectationsWereMet())
}
