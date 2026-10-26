package damengnative

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeGeneratedColumnConstantChangesOnlyExactExpression(t *testing.T) {
	ddl := `CREATE TABLE "App"."T"("(" VARCHAR(50) DEFAULT 'AS(x), VIRTUAL', "MixedKey" DECIMAL(18,4) GENERATED ALWAYS AS (CASE WHEN "("=')' THEN 15 ELSE ("Input"*"Input") END) VIRTUAL NOT NULL /* keep AS('x') */,"Input" INT, CONSTRAINT "Check" CHECK("MixedKey">0)) PARTITION BY RANGE("MixedKey") INTERVAL(10)(PARTITION "P" VALUES LESS THAN(0))`
	declaration, err := NativeGeneratedColumnDeclaration(ddl, "MixedKey")
	require.NoError(t, err)
	require.Equal(t, `"MixedKey" DECIMAL(18,4) GENERATED ALWAYS AS (CASE WHEN "("=')' THEN 15 ELSE ("Input"*"Input") END) VIRTUAL NOT NULL`, declaration)
	changed, err := RewriteNativeGeneratedColumnConstant(ddl, "MixedKey", "CAST(15 AS DECIMAL(18,4))")
	require.NoError(t, err)
	require.Equal(t, `CREATE TABLE "App"."T"("(" VARCHAR(50) DEFAULT 'AS(x), VIRTUAL', "MixedKey" DECIMAL(18,4) GENERATED ALWAYS AS (CAST(15 AS DECIMAL(18,4))) VIRTUAL NOT NULL /* keep AS('x') */,"Input" INT, CONSTRAINT "Check" CHECK("MixedKey">0)) PARTITION BY RANGE("MixedKey") INTERVAL(10)(PARTITION "P" VALUES LESS THAN(0))`, changed)
	_, err = NativeGeneratedColumnDeclaration(ddl, "MIXEDKEY")
	require.Error(t, err, "quoted identifier case cannot be guessed")
	for _, unsafe := range []string{`"App"."Danger"()`, "Id", "NEXTVAL", "SELECT 1", "1); DROP TABLE T;--", "?", "CURRENT_TIMESTAMP"} {
		_, err = RewriteNativeGeneratedColumnConstant(ddl, "MixedKey", unsafe)
		require.Error(t, err, unsafe)
	}
}

func TestGeneratedIntervalPlanMappingAndPersistenceKeepsRecipe(t *testing.T) {
	ddl := `CREATE TABLE "App"."T"("Input" INT,"Key" INT GENERATED ALWAYS AS ("Input"*"Input") VIRTUAL)`
	p := &IntervalPartitionPlan{Column: "Key", GeneratedKey: true, DefinitionSQL: ddl, EmptyTouch: &MaterializedEmptyTouch{Table: "T", DefinitionSQL: ddl, Columns: []MaterializedEmptyColumn{{Name: "Input"}}}, Partitions: []IntervalPartition{{Name: "Archive", HighSQL: "30", SeedSQL: "CAST(29 AS INT)"}}}
	mapped, err := RewriteIntervalPartitions(p, "App", "ExactTarget")
	require.NoError(t, err)
	require.True(t, mapped.GeneratedKey)
	require.Contains(t, mapped.EmptyTouch.DefinitionSQL, `"ExactTarget"."T"`)
	require.Contains(t, p.DefinitionSQL, `"App"."T"`)
	encoded, err := json.Marshal(mapped)
	require.NoError(t, err)
	var reload IntervalPartitionPlan
	require.NoError(t, json.Unmarshal(encoded, &reload))
	require.Equal(t, Fingerprint(mapped), Fingerprint(&reload))
	before := IntervalPartitionSQL("ExactTarget", "T", mapped)
	mapped.GeneratedKey = false
	require.NotEqual(t, before, IntervalPartitionSQL("ExactTarget", "T", mapped))
}
