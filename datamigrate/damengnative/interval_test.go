package damengnative

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntervalPlanMappingPreservesLiteralsAndSurvivesReload(t *testing.T) {
	original := &IntervalPartitionPlan{Column: "MixedId", DefinitionSQL: `CREATE TABLE "Source"."T"("MixedId" INT)`, EmptyTouch: &MaterializedEmptyTouch{Table: "T", DefinitionSQL: `CREATE TABLE "Source"."T"("MixedId" INT)`, Columns: []MaterializedEmptyColumn{{Name: "MixedId", DefaultSQL: `"Source"."Seq".NEXTVAL`, ValueSQL: `CAST('Source.T' AS VARCHAR(20))`}}, Constraints: []MaterializedEmptyConstraint{{Table: "T", Name: "Pk", Columns: []string{"MixedId"}, DefinitionSQL: `ALTER TABLE "Source"."T" ADD CONSTRAINT "Pk" PRIMARY KEY("MixedId")`}}, IdentityRestore: []string{`ALTER TABLE "Source"."T" DROP IDENTITY`}}, Partitions: []IntervalPartition{{Name: "ExactCase", HighSQL: "25", SeedSQL: "24", Tablespace: "MainCase", DefinitionSQL: `CREATE TABLE "Source"."Child"("MixedId" INT)`}}}
	original.Partitions[0].DerivedConstraintNames = []string{"NativePk"}
	original.Partitions[0].Values = []IntervalSeedValue{{Column: "ExactCase", SQL: `CAST('Source.T' AS VARCHAR(40))`}, {Column: "Other", SQL: `CAST(0 AS "Source"."DecimalDomain")`}}
	mapped, err := RewriteIntervalPartitions(original, "Source", `Target"Case`)
	require.NoError(t, err)
	require.Contains(t, mapped.DefinitionSQL, `"Target""Case"."T"`)
	require.Equal(t, `CAST('Source.T' AS VARCHAR(20))`, mapped.EmptyTouch.Columns[0].ValueSQL)
	require.Equal(t, `"Target""Case"."Seq".NEXTVAL`, mapped.EmptyTouch.Columns[0].DefaultSQL)
	require.Equal(t, "ExactCase", mapped.Partitions[0].Name)
	require.Equal(t, "MainCase", mapped.Partitions[0].Tablespace)
	require.Equal(t, `CAST('Source.T' AS VARCHAR(40))`, mapped.Partitions[0].Values[0].SQL)
	require.Equal(t, `CAST(0 AS "Target""Case"."DecimalDomain")`, mapped.Partitions[0].Values[1].SQL)
	encoded, err := json.Marshal(mapped)
	require.NoError(t, err)
	var loaded IntervalPartitionPlan
	require.NoError(t, json.Unmarshal(encoded, &loaded))
	require.Equal(t, IntervalPartitionSQL(`Target"Case`, "T", mapped), IntervalPartitionSQL(`Target"Case`, "T", &loaded))
	mapped.EmptyTouch.Constraints[0].Columns[0] = "Changed"
	mapped.Partitions[0].Name = "Changed"
	mapped.Partitions[0].DerivedConstraintNames[0] = "Changed"
	mapped.Partitions[0].Values[0].SQL = "Changed"
	require.Equal(t, "MixedId", original.EmptyTouch.Constraints[0].Columns[0])
	require.Equal(t, "ExactCase", original.Partitions[0].Name)
	require.Equal(t, "NativePk", original.Partitions[0].DerivedConstraintNames[0])
	require.Equal(t, `CAST('Source.T' AS VARCHAR(40))`, original.Partitions[0].Values[0].SQL)
}
