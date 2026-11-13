package cdc

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/stretchr/testify/require"
)

func TestDamengUnsupportedEventsNeverCommitBufferedRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  replication.EventType
		event replication.Event
	}{
		{"compressed generic", replication.TRANSACTION_PAYLOAD_EVENT, &replication.GenericEvent{}},
		{"xa prepare generic", replication.XA_PREPARE_LOG_EVENT, &replication.GenericEvent{}},
		{"xa commit", replication.QUERY_EVENT, &replication.QueryEvent{Query: []byte("XA COMMIT 'x'")}},
		{"xa rollback", replication.QUERY_EVENT, &replication.QueryEvent{Query: []byte("/* comment */ XA ROLLBACK 'x'")}},
		{"xa executable comment", replication.QUERY_EVENT, &replication.QueryEvent{Query: []byte("/*!80000 XA START 'x' */")}},
		{"xa adjacent comment", replication.QUERY_EVENT, &replication.QueryEvent{Query: []byte("XA/*comment*/COMMIT 'x' ONE PHASE")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := Position{File: "mysql-bin.000001", Pos: 123}
			state := newTransactionState(Config{TargetDBType: "dameng"}, nil, before)
			state.Changes = []Change{{Action: "insert", After: []any{1}}}
			result, err := state.Consume(&replication.BinlogEvent{Header: &replication.EventHeader{EventType: tc.kind, LogPos: 456}, Event: tc.event})
			require.ErrorContains(t, err, "原检查点保留")
			require.False(t, result.Commit)
			require.Empty(t, state.Changes)
			require.Equal(t, before, state.Position)
		})
	}
}
func TestTransactionStateCommitRollbackAndDDL(t *testing.T) {
	state := newTransactionState(Config{TargetDBType: "dameng", SourceDatabase: "SourceCase"}, nil, Position{File: "bin.000001", Pos: 4})
	state.Changes = []Change{{Action: "insert"}}
	event := func(query string, pos uint32) transactionEvent {
		out, err := state.Consume(&replication.BinlogEvent{Header: &replication.EventHeader{EventType: replication.QUERY_EVENT, LogPos: pos}, Event: &replication.QueryEvent{Schema: []byte("SourceCase"), Query: []byte(query)}})
		require.NoError(t, err)
		return out
	}
	require.False(t, event("BEGIN", 10).Commit)
	require.Len(t, state.Changes, 1)
	require.True(t, event("ROLLBACK", 20).Commit)
	require.Empty(t, state.Changes)
	require.NotEmpty(t, event("/* migration */ ALTER TABLE t MODIFY b INT FIRST", 30).DDL)
	require.NotEmpty(t, event("ALTER/* migration */TABLE t DROP COLUMN b", 40).DDL)
	require.True(t, sourceDDL("RENAME TABLE `SourceCase`.`t` TO `elsewhere`.`t`", "elsewhere", "SourceCase"))
	require.False(t, sourceDDL("ALTER TABLE t ADD x INT", "elsewhere", "SourceCase"))
}

func TestHistoricalIntervalRequiresWholeTransactions(t *testing.T) {
	event := func(kind replication.EventType, query string) *replication.BinlogEvent {
		return &replication.BinlogEvent{Header: &replication.EventHeader{EventType: kind}, Event: &replication.QueryEvent{Query: []byte(query)}}
	}
	for _, kind := range []replication.EventType{replication.TABLE_MAP_EVENT, replication.WRITE_ROWS_EVENTv2, replication.XID_EVENT} {
		state := intervalTransactionState{}
		require.ErrorContains(t, state.consume(event(kind, "")), "事务内部")
	}
	state := intervalTransactionState{}
	for _, ev := range []*replication.BinlogEvent{
		event(replication.GTID_EVENT, ""), event(replication.QUERY_EVENT, "BEGIN"),
		event(replication.TABLE_MAP_EVENT, ""), event(replication.WRITE_ROWS_EVENTv2, ""), event(replication.XID_EVENT, ""),
		event(replication.ANONYMOUS_GTID_EVENT, ""), event(replication.QUERY_EVENT, "ALTER TABLE other.t ADD x INT"),
	} {
		require.NoError(t, state.consume(ev))
	}
	require.False(t, state.open)
}
