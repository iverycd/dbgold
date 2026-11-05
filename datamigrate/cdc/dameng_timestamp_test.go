package cdc

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDamengTimestampUsesSourceSessionForBeforeAndAfter(t *testing.T) {
	db, mock := newSequenceMock(t)
	table := &TableInfo{Name: "Events", Columns: []string{"stamp", "wall", "zero"}, ColumnTypes: []string{"TIMESTAMP", "DATETIME", "TIMESTAMP"}}
	before := []any{"2024-01-02 03:04:05.123456", "2024-01-02 03:04:05.123456", "0000-00-00 00:00:00"}
	after := []any{"2024-01-02 03:04:05.123456", "2024-05-06 07:08:09.123456", nil}
	instant, err := time.Parse("2006-01-02 15:04:05.999999", before[0].(string))
	require.NoError(t, err)
	epoch := fmt.Sprintf("%d.123456", instant.Unix())
	mock.ExpectQuery("SELECT " + damengSourceTimestampExpression).WithArgs(epoch).WillReturnRows(sqlmock.NewRows([]string{"wall"}).AddRow("2024-01-02 11:04:05.123456"))
	changes, err := normalizeDamengTimestamps(context.Background(), db, []Change{{Action: "update", Table: table, Before: before, After: after}})
	require.NoError(t, err)
	require.Equal(t, "2024-01-02 11:04:05.123456", changes[0].Before[0])
	require.Equal(t, changes[0].Before[0], changes[0].After[0])
	require.Equal(t, before[1:], changes[0].Before[1:])
	require.Equal(t, after[1:], changes[0].After[1:])
	require.Equal(t, "2024-01-02 03:04:05.123456", before[0], "replication-owned rows must not mutate")
}

func TestDamengTimestampBatchesDistinctInstants(t *testing.T) {
	db, mock := newSequenceMock(t)
	table := &TableInfo{Name: "T", Columns: []string{"ts"}, ColumnTypes: []string{"TIMESTAMP"}}
	var changes []Change
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < damengTimestampBatchSize+1; i++ {
		changes = append(changes, Change{Action: "insert", Table: table, After: []any{base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05.000000")}})
	}
	for start := 0; start < len(changes); start += damengTimestampBatchSize {
		count := min(damengTimestampBatchSize, len(changes)-start)
		cols := make([]string, count)
		values := make([]any, count)
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
			values[i] = changes[start+i].After[0]
		}
		row := sqlmock.NewRows(cols)
		// sqlmock's AddRow takes database/sql/driver.Value rather than any.
		query := mock.ExpectQuery("SELECT " + strings.TrimSuffix(strings.Repeat(damengSourceTimestampExpression+",", count), ","))
		if count == 1 {
			row.AddRow(values[0])
		} else {
			raw := make([]driver.Value, count)
			for i := range raw {
				raw[i] = values[i]
			}
			row.AddRow(raw...)
		}
		query.WillReturnRows(row)
	}
	got, err := normalizeDamengTimestamps(context.Background(), db, changes)
	require.NoError(t, err)
	require.Equal(t, changes, got)
}

func TestDamengTimestampRejectsUnrepresentableSourceInstant(t *testing.T) {
	db, mock := newSequenceMock(t)
	mock.ExpectQuery("SELECT " + damengSourceTimestampExpression).WillReturnRows(sqlmock.NewRows([]string{"wall"}).AddRow(nil))
	_, err := normalizeDamengTimestamps(context.Background(), db, []Change{{Action: "insert", Table: &TableInfo{Name: "T", Columns: []string{"ts"}, ColumnTypes: []string{"TIMESTAMP"}}, After: []any{"2024-01-01 00:00:00"}}})
	require.ErrorContains(t, err, "无法表示 TIMESTAMP")
}
