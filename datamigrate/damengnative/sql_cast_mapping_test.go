package damengnative

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSchemaMappingRecognizesQualifiedCastTypeWithoutDiscardingAliasProof(t *testing.T) {
	for _, text := range []string{`CAST('Source.T' AS "Source"."NumericDomain")`, `SELECT CAST(CAST("Source"."T"."C" AS INT) AS "Source"."NumericDomain") FROM "Source"."T"`} {
		mapped, _, err := RewriteSchema(text, "Source", "Target")
		require.NoError(t, err)
		require.Contains(t, mapped, `AS "Target"."NumericDomain"`)
	}
	for _, text := range []string{
		`SELECT "Source"."C" AS "Source" FROM "Other"."T"`,
		`SELECT CAST(1 AS "Source"."NumericDomain"),"Source"."C" FROM "Other"."T" AS "Source"`,
		`SELECT CAST(1 AS "Source"."NumericDomain") AS "Source","Source"."C" FROM "Other"."T"`,
		`SELECT f(1 AS "Source"."NumericDomain"),"Source"."C" FROM "Other"."T"`,
	} {
		_, _, err := RewriteSchema(text, "Source", "Target")
		require.ErrorContains(t, err, "别名", text)
	}
}
