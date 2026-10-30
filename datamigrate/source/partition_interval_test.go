package source

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNativeIntervalConstantsAndSparseLowerBounds(t *testing.T) {
	for _, tc := range []struct{ raw, unit, amount string }{{"10", "number", "10"}, {"NUMTOYMINTERVAL(2,'YEAR')", "month", "24"}, {"NUMTODSINTERVAL(.5,'DAY')", "second", "43200"}, {"  NUMTODSINTERVAL(0.0000010,'SECOND')  ", "second", "0.000001"}} {
		r, e := ParsePartitionInterval(tc.raw)
		require.NoError(t, e)
		require.Equal(t, tc.unit, r.Unit)
		require.Equal(t, tc.amount, r.Amount)
	}
	for _, bad := range []string{"0", "-1", "foo()", "NUMTODSINTERVAL(1,'YEAR')", "NUMTOYMINTERVAL(1.5,'MONTH')", "NUMTODSINTERVAL(0.00000001,'SECOND')", "1; DELETE FROM t"} {
		_, e := ParsePartitionInterval(bad)
		require.Error(t, e, bad)
	}
	s := &PartitionSpec{Method: "RANGE", Automatic: "10", Keys: []PartitionKey{{Column: "MiXeD"}}, Partitions: []Partition{{Name: "Initial", Upper: []PartitionValue{{Kind: "number", Text: "0"}}}, {Name: "Later", Upper: []PartitionValue{{Kind: "number", Text: "50"}}}, {Name: "Earlier", Upper: []PartitionValue{{Kind: "number", Text: "30"}}}}}
	require.NoError(t, catalogFreezeInterval(s, map[string]bool{"Initial": true}))
	require.Equal(t, "40", s.Partitions[1].Lower[0].Text)
	require.Equal(t, "20", s.Partitions[2].Lower[0].Text)
	require.Equal(t, PartitionValue{Kind: "number", Text: "0"}, s.Interval.Transition)
	require.True(t, s.Partitions[1].IsInterval)
	clone := ClonePartitionSpec(s)
	clone.Interval.Amount = "20"
	require.Equal(t, "10", s.Interval.Amount)
}

func TestNativeIntervalDateGridAndDamengInitialNames(t *testing.T) {
	oracle, e := catalogPartitionValue("TIMESTAMP' 2024-01-01 00:00:00.000001'")
	require.NoError(t, e)
	require.Equal(t, "2024-01-01 00:00:00.000001", oracle.Text)
	value, e := catalogPartitionValue("DATETIME'2024-04-01 00:00:00'")
	require.NoError(t, e)
	require.Equal(t, "timestamp", value.Kind)
	s := &PartitionSpec{Method: "RANGE", Automatic: "NUMTOYMINTERVAL(1,'MONTH')", Keys: []PartitionKey{{Column: "At"}}, Partitions: []Partition{{Name: "Before", Upper: []PartitionValue{{Kind: "timestamp", Text: "2024-01-01 00:00:00"}}}, {Name: "MiXeD", Upper: []PartitionValue{value}}}}
	require.NoError(t, catalogDamengInterval(s, `CREATE TABLE "T"("At" TIMESTAMP) PARTITION BY RANGE("At") INTERVAL(NUMTOYMINTERVAL(1,'MONTH')) (PARTITION "Before" VALUES LESS THAN(DATETIME'2024-01-01 00:00:00'))`))
	require.Equal(t, "2024-03-01 00:00:00", s.Partitions[1].Lower[0].Text)
	_, e = IntervalBoundaryIndex(s.Interval, PartitionValue{Kind: "timestamp", Text: "2024-04-02 00:00:00"})
	require.Error(t, e)
	s.Partitions[1].Upper[0].Text = "2024-03-31 00:00:00"
	require.Error(t, catalogFreezeInterval(s, map[string]bool{"Before": true}))
}
