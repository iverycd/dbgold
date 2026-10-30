package partition

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func compilerSQLServerLeftFixture(dataType string, bounds ...string) *source.TableDDLInfo {
	info := &source.TableDDLInfo{TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Id", DataType: dataType}}, Partition: &source.PartitionSpec{Method: "RANGE", Algorithm: "sqlserver-range:OriginalFunction", RowMovement: "enabled", Keys: []source.PartitionKey{{Column: "Id", DataType: dataType}}}}
	low := source.PartitionValue{Kind: "minvalue"}
	for i := 0; i <= len(bounds); i++ {
		high := source.PartitionValue{Kind: "maxvalue"}
		if i < len(bounds) {
			high = source.PartitionValue{Kind: "number", Text: bounds[i]}
		}
		info.Partition.Partitions = append(info.Partition.Partitions, source.Partition{Name: fmt.Sprintf("Leaf%d", i), Ordinal: i + 1, Schema: "Src", Lower: []source.PartitionValue{low}, Upper: []source.PartitionValue{high}, UpperInclusive: true})
		low = high
	}
	return info
}

func TestSQLServerIntegerRangeLeftCompilesToExactHalfOpenNativeRanges(t *testing.T) {
	for _, target := range []string{"postgres", "mysql", "oracle", "dameng"} {
		t.Run(target, func(t *testing.T) {
			info := compilerSQLServerLeftFixture("bigint", "-9223372036854775808", "-1", "0", "9223372036854775806")
			original := source.ClonePartitionSpec(info.Partition)
			plan, err := Compile(info, compilerBase(t, info, target), compilerProfile(target), Options{SourceType: "sqlserver", TargetType: target, TargetSchema: "Dst"})
			require.NoError(t, err)
			require.Equal(t, original, source.ClonePartitionSpec(info.Partition))
			require.Equal(t, original, plan.Source)
			require.Contains(t, plan.Rule, "sqlserver-integer-range-left-to-half-open")
			require.Len(t, plan.Mapping, 5)
			for i, bound := range []string{"-9223372036854775807", "0", "1", "9223372036854775807"} {
				require.Equal(t, source.PartitionValue{Kind: "number", Text: bound}, plan.Target.Partitions[i].Upper[0])
				require.Equal(t, plan.Target.Partitions[i].Upper, plan.Target.Partitions[i+1].Lower)
			}
			require.Equal(t, "minvalue", plan.Target.Partitions[0].Lower[0].Kind)
			require.Equal(t, "maxvalue", plan.Target.Partitions[4].Upper[0].Kind)
			for _, p := range plan.Target.Partitions {
				require.True(t, p.LowerInclusive)
				require.False(t, p.UpperInclusive)
			}
			ddl := dialect.JoinSQL(plan.Statements)
			if target == "postgres" {
				require.Contains(t, ddl, "FOR VALUES FROM (MINVALUE) TO (-9223372036854775807)")
			} else {
				require.Contains(t, ddl, "VALUES LESS THAN (-9223372036854775807)")
			}
			for _, value := range []int64{math.MinInt64, math.MinInt64 + 1, -2, -1, 0, 1, math.MaxInt64 - 1, math.MaxInt64} {
				require.Equal(t, partitionIntegerOwner(t, original, value), partitionIntegerOwner(t, plan.Target, value), "key %d", value)
			}
		})
	}
}

func partitionIntegerOwner(t *testing.T, spec *source.PartitionSpec, value int64) int {
	t.Helper()
	for i, p := range spec.Partitions {
		inside := true
		if p.Lower[0].Kind == "number" {
			low, err := strconv.ParseInt(p.Lower[0].Text, 10, 64)
			require.NoError(t, err)
			inside = value > low || value == low && p.LowerInclusive
		}
		if p.Upper[0].Kind == "number" {
			high, err := strconv.ParseInt(p.Upper[0].Text, 10, 64)
			require.NoError(t, err)
			inside = inside && (value < high || value == high && p.UpperInclusive)
		}
		if inside {
			return i
		}
	}
	t.Fatalf("key %d has no leaf", value)
	return -1
}

func TestSQLServerRangeLeftPreservesEntireTinyintDomain(t *testing.T) {
	info := compilerSQLServerLeftFixture("tinyint", "0", "1", "127", "254")
	plan, err := Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "sqlserver", TargetType: "postgres", TargetSchema: "Dst"})
	require.NoError(t, err)
	for value := int64(0); value <= 255; value++ {
		require.Equal(t, partitionIntegerOwner(t, info.Partition, value), partitionIntegerOwner(t, plan.Target, value))
	}
}

func TestSQLServerRangeLeftMaximumBoundaryDoesNotDropEmptyLeavesOrOverflow(t *testing.T) {
	for _, entry := range []struct{ kind, maximum string }{{"tinyint", "255"}, {"smallint", "32767"}, {"int", "2147483647"}, {"bigint", "9223372036854775807"}} {
		for _, target := range []string{"postgres", "mysql", "oracle", "dameng"} {
			t.Run(entry.kind+"/"+target, func(t *testing.T) {
				info := compilerSQLServerLeftFixture(entry.kind, entry.maximum)
				plan, err := Compile(info, compilerBase(t, info, target), compilerProfile(target), Options{SourceType: "sqlserver", TargetType: target, TargetSchema: "Dst"})
				require.ErrorContains(t, err, "空叶分区")
				require.Nil(t, plan)
			})
		}
	}
}

func TestSQLServerRangeLeftRejectsUnprovenDomainsAndMalformedBounds(t *testing.T) {
	for _, scenario := range []string{"float", "decimal", "datetime", "varchar", "fraction", "outside domain", "null", "mixed", "expression", "key type mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			info := compilerSQLServerLeftFixture("int", "0")
			switch scenario {
			case "float", "decimal", "datetime", "varchar":
				info.Columns[0].DataType, info.Partition.Keys[0].DataType = scenario, scenario
			case "fraction":
				info.Partition.Partitions[0].Upper[0].Text = "0.5"
			case "outside domain":
				info.Partition.Partitions[0].Upper[0].Text = "-2147483649"
			case "null":
				info.Partition.Partitions[0].Upper[0] = source.PartitionValue{Kind: "null"}
			case "mixed":
				info.Partition.Partitions[1].LowerInclusive = true
			case "expression":
				info.Partition.Keys[0].Column, info.Partition.Keys[0].Expression = "", "Id+1"
			case "key type mismatch":
				info.Partition.Keys[0].DataType = "float"
			}
			plan, err := Compile(info, []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("Id" int)`}}, compilerProfile("postgres"), Options{SourceType: "sqlserver", TargetType: "postgres", TargetSchema: "Dst"})
			require.Error(t, err)
			require.Nil(t, plan)
		})
	}
}

func TestSQLServerRangeLeftKeepsMySQLNullRouting(t *testing.T) {
	info := compilerSQLServerLeftFixture("int", "10")
	info.Columns[0].IsNullable = true
	plan, err := Compile(info, compilerBase(t, info, "mysql"), compilerProfile("mysql"), Options{SourceType: "sqlserver", TargetType: "mysql", TargetSchema: "Dst"})
	require.NoError(t, err)
	require.Equal(t, "11", plan.Target.Partitions[0].Upper[0].Text)
	for _, target := range []string{"postgres", "oracle", "dameng"} {
		_, err := Compile(info, compilerBase(t, info, target), compilerProfile(target), Options{SourceType: "sqlserver", TargetType: target, TargetSchema: "Dst"})
		require.ErrorContains(t, err, "NULL")
	}
}
