package partition

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func compilerLinearHashFixture(n int) *source.TableDDLInfo {
	info := compilerRangeFixture()
	info.Columns[0].DataType = "bigint"
	info.Columns[0].IsNullable = true
	info.Partition.Method, info.Partition.Algorithm = "LINEAR HASH", "mysql-linear-hash"
	info.Partition.Keys[0].DataType = "bigint"
	info.Partition.Partitions = nil
	for i := 0; i < n; i++ {
		info.Partition.Partitions = append(info.Partition.Partitions, source.Partition{Schema: "Src", Name: fmt.Sprintf("MiXeD_%d", i), Ordinal: i + 1, HashModulus: n, HashRemainder: i})
	}
	return info
}

func TestMySQLLinearHashPreservesNativeMethodAndBucketIdentity(t *testing.T) {
	info := compilerLinearHashFixture(6)
	plan, err := Compile(info, compilerBase(t, info, "mysql"), compilerProfile("mysql"), Options{SourceType: "mysql", TargetType: "mysql", TargetSchema: "Dst"})
	require.NoError(t, err)
	require.Equal(t, "LINEAR HASH", plan.Target.Method)
	require.Equal(t, "mysql-linear-hash", plan.Target.Algorithm)
	require.Contains(t, dialect.JoinSQL(plan.Statements), "PARTITION BY LINEAR HASH (`Id`)")
	require.Len(t, plan.Mapping, 6)
	for i, p := range plan.Target.Partitions {
		require.Equal(t, i, p.HashRemainder)
		require.Equal(t, 6, p.HashModulus)
		require.Equal(t, info.Partition.Partitions[i].Name, p.Name)
	}
	info.Partition.Algorithm = "mysql-mod"
	_, err = Compile(info, compilerBase(t, info, "mysql"), compilerProfile("mysql"), Options{SourceType: "mysql", TargetType: "mysql", TargetSchema: "Dst"})
	require.ErrorContains(t, err, "算法")
}

func TestMySQLLinearHashNativeListKeepsEverySignedResidueAndFutureKeyValue(t *testing.T) {
	// The independent oracle is MySQL's native mask/fold algorithm, whereas
	// the generated target routes signed MOD residues through LIST constants.
	for n := 1; n <= 64; n++ {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			info := compilerLinearHashFixture(n)
			plan, err := Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst"})
			require.NoError(t, err)
			require.Equal(t, "LIST", plan.Target.Method)
			require.Contains(t, plan.Rule, "mysql-linear-mask-to-native-list")
			require.Equal(t, "LINEAR HASH", plan.Source.Method)
			modulus := int64(1)
			for modulus < int64(n) {
				modulus <<= 1
			}
			expression := `pg_catalog.mod(COALESCE(CAST("Id" AS numeric),CAST('-9223372036854775808' AS numeric)),CAST('` + strconv.FormatInt(modulus, 10) + `' AS numeric))`
			require.Equal(t, expression, plan.Target.Keys[0].Expression)
			residues := map[int64]int{}
			for bucket, p := range plan.Target.Partitions {
				for _, tuple := range p.Values {
					require.Len(t, tuple, 1)
					r, e := strconv.ParseInt(tuple[0].Text, 10, 64)
					require.NoError(t, e)
					_, exists := residues[r]
					require.False(t, exists, "LIST residues may not overlap")
					residues[r] = bucket
				}
			}
			require.Len(t, residues, int(2*modulus-1))
			values := []int64{math.MinInt64, math.MinInt64 + 1, math.MaxInt64, 1998, 2003}
			for value := -2 * modulus; value <= 2*modulus; value++ {
				values = append(values, value)
			}
			for _, value := range values {
				want := uint64(value) & uint64(modulus-1)
				if want >= uint64(n) {
					want = uint64(value) & uint64(modulus/2-1)
				}
				got, exists := residues[value%modulus]
				require.True(t, exists)
				require.Equal(t, int(want), got, "value %d must keep its physical leaf", value)
			}
			require.Equal(t, 0, residues[math.MinInt64%modulus], "MySQL NULL uses LLONG_MIN and therefore the first linear bucket")
		})
	}
}

func TestMySQLLinearHashListCatalogDeparseIsEquivalent(t *testing.T) {
	info := compilerLinearHashFixture(3)
	plan, err := Compile(info, compilerBase(t, info, "postgres"), compilerProfile("postgres"), Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst", ColumnName: func(string) string { return `MiXeD"Key` }})
	require.NoError(t, err)
	actual := source.ClonePartitionSpec(plan.Target)
	actual.Keys[0].Expression = `mod(COALESCE(("MiXeD""Key")::numeric, '-9223372036854775808'::numeric), '4'::numeric)`
	require.NoError(t, Equivalent(plan.Target, actual))
	actual.Keys[0].Expression = strings.ReplaceAll(actual.Keys[0].Expression, "'4'", "'3'")
	require.Error(t, Equivalent(plan.Target, actual))
}

func TestMySQLLinearHashKeepsUnprovenFormsBlocked(t *testing.T) {
	for _, scenario := range []string{"wrong algorithm", "unsigned", "expression", "foreign hash", "no native list", "no native expression", "bad bucket", "unknown linear method"} {
		t.Run(scenario, func(t *testing.T) {
			info := compilerLinearHashFixture(3)
			profile := compilerProfile("postgres")
			options := Options{SourceType: "mysql", TargetType: "postgres", TargetSchema: "Dst"}
			switch scenario {
			case "wrong algorithm":
				info.Partition.Algorithm = "mysql-mod"
			case "unsigned":
				info.Partition.Keys[0].DataType = "bigint unsigned"
			case "expression":
				info.Partition.Keys[0].Column, info.Partition.Keys[0].Expression = "", "ABS(Id)"
			case "foreign hash":
				options.SourceType = "oracle"
			case "no native list":
				profile.Features[FeatureList] = false
			case "no native expression":
				profile.Features[FeatureExpressions] = false
			case "bad bucket":
				info.Partition.Partitions[1].HashRemainder = 0
			case "unknown linear method":
				info.Partition.Method = "LINEAR RANGE"
			}
			plan, err := Compile(info, compilerBase(t, info, "postgres"), profile, options)
			require.Error(t, err)
			require.Nil(t, plan)
		})
	}
}

func TestMySQLLinearHashSubpartitionsKeepSeparateNamedLeaves(t *testing.T) {
	for _, product := range []string{"mysql", "postgres"} {
		t.Run(product, func(t *testing.T) {
			info := compilerRangeFixture()
			for i := range info.Partition.Partitions {
				sub := compilerLinearHashFixture(3).Partition
				for j := range sub.Partitions {
					sub.Partitions[j].Name = fmt.Sprintf("Parent%d_Leaf%d", i, j)
				}
				info.Partition.Partitions[i].Subpartition = sub
			}
			profile := compilerProfile(product)
			profile.Features[FeatureSubpartition] = true
			plan, err := Compile(info, compilerBase(t, info, product), profile, Options{SourceType: "mysql", TargetType: product, TargetSchema: "Dst"})
			require.NoError(t, err)
			require.Len(t, plan.Mapping, 6)
			ddl := dialect.JoinSQL(plan.Statements)
			if product == "mysql" {
				require.Contains(t, ddl, "SUBPARTITION BY LINEAR HASH (`Id`)")
			} else {
				require.Contains(t, ddl, "PARTITION BY LIST (pg_catalog.mod(")
			}
		})
	}
}
