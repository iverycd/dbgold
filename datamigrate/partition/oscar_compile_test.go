package partition

import (
	"testing"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func oscarCompilerProfile() Profile {
	p := compilerProfile("oscar")
	p.Features["inline_oracle"], p.Features["row_movement"], p.Features[FeatureDefaultList] = true, true, true
	return p
}

func TestOscarNativeListDefaultIsLastWithoutChangingSourceNamesOrMapping(t *testing.T) {
	info := compilerListFixture()
	info.Partition.Partitions[0].Default, info.Partition.Partitions[0].Values = true, nil
	original := source.ClonePartitionSpec(info.Partition)
	base := []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("Id" int)`}}
	plan, err := Compile(info, base, oscarCompilerProfile(), Options{SourceType: "oracle", TargetType: "oscar", TargetSchema: "Dst"})
	require.NoError(t, err)
	require.Equal(t, original, source.ClonePartitionSpec(info.Partition))
	require.Equal(t, original, plan.Source)
	require.Equal(t, "P_Rest", plan.Target.Partitions[0].Name)
	require.Equal(t, 1, plan.Target.Partitions[0].Ordinal)
	require.Equal(t, "P_Neg", plan.Target.Partitions[1].Name)
	require.True(t, plan.Target.Partitions[1].Default)
	require.Equal(t, 2, plan.Target.Partitions[1].Ordinal)
	require.Equal(t, "P_Neg", plan.Mapping[0].SourceName)
	require.Equal(t, "P_Neg", plan.Mapping[0].TargetName)
	require.Contains(t, dialect.JoinSQL(plan.Statements), "PARTITION \"P_Rest\" VALUES (10),\n  PARTITION \"P_Neg\" VALUES (DEFAULT)")
	info.Partition.Partitions[1].Default, info.Partition.Partitions[1].Values = true, nil
	_, err = Compile(info, base, oscarCompilerProfile(), Options{SourceType: "oracle", TargetType: "oscar", TargetSchema: "Dst"})
	require.ErrorContains(t, err, "多个 DEFAULT")
}

func TestOscarNullableRangePreservesOracleMaxvalueAndRejectsOtherNullRoutes(t *testing.T) {
	info := compilerRangeFixture()
	info.Columns[0].IsNullable = true
	base := []dialect.Statement{{SQL: `CREATE TABLE "Dst"."Orders" ("Id" int)`}}
	for _, src := range []string{"oracle", "dameng"} {
		_, err := Compile(info, base, oscarCompilerProfile(), Options{SourceType: src, TargetType: "oscar", TargetSchema: "Dst"})
		require.NoError(t, err)
	}
	_, err := Compile(info, base, oscarCompilerProfile(), Options{SourceType: "mysql", TargetType: "oscar", TargetSchema: "Dst"})
	require.ErrorContains(t, err, "NULL")
	info.Partition.RowMovement = "disabled"
	_, err = Compile(info, base, oscarCompilerProfile(), Options{SourceType: "oracle", TargetType: "oscar", TargetSchema: "Dst"})
	require.ErrorContains(t, err, "UPDATE")
}
