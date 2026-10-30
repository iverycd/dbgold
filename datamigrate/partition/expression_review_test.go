package partition

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestRoutingCanonicalPreservesQuotedFunctionIdentity(t *testing.T) {
	builtin := CanonicalKey(source.PartitionKey{Expression: `pg_catalog.abs(CAST("x" AS numeric))`})
	require.Equal(t, builtin, CanonicalKey(source.PartitionKey{Expression: `pg_catalog."abs"(("x")::numeric)`}))
	for _, expression := range []string{
		`pg_catalog."ABS"(CAST("x" AS numeric))`,
		`"PG_CATALOG".abs(CAST("x" AS numeric))`,
		`"ABS"(CAST("x" AS numeric))`,
		`other.abs(CAST("x" AS numeric))`,
	} {
		require.NotEqual(t, builtin, CanonicalKey(source.PartitionKey{Expression: expression}), expression)
	}
}

func TestRoutingCanonicalSeparatesIdentifierBytesFromASTDelimiters(t *testing.T) {
	left := source.PartitionKey{Expression: `(CAST("a),cast:numeric(column:b" AS numeric) + CAST("c" AS numeric))`}
	right := source.PartitionKey{Expression: `(CAST("a" AS numeric) + CAST("b),cast:numeric(column:c" AS numeric))`}
	require.NotEqual(t, CanonicalKey(left), CanonicalKey(right), "different legal column pairs must not have the same routing fingerprint")
}

func TestRoutingCanonicalCoalesceSyntaxIsNotAUserFunction(t *testing.T) {
	builtin := CanonicalKey(source.PartitionKey{Expression: `COALESCE(CAST("x" AS numeric),CAST('-9223372036854775808' AS numeric))`})
	for _, expression := range []string{
		`"coalesce"(("x")::numeric,'-9223372036854775808'::numeric)`,
		`pg_catalog.coalesce(("x")::numeric,'-9223372036854775808'::numeric)`,
		`pg_catalog."coalesce"(("x")::numeric,'-9223372036854775808'::numeric)`,
	} {
		require.NotEqual(t, builtin, CanonicalKey(source.PartitionKey{Expression: expression}), expression)
	}
}

func TestIntegerRoutingRejectsMySQLDivisionMinimumQuotient(t *testing.T) {
	c := integerExpressionCompiler()
	for _, expression := range []string{
		"wide DIV 1",
		"(wide + 0) DIV 1",
		"(-9223372036854775807 - 1) DIV 1",
	} {
		_, err := c.integerExpression(expression)
		require.ErrorContains(t, err, "DIV 负商可能达到 BIGINT 最小值", expression)
	}
	for _, expression := range []string{"wide DIV 2", "wide DIV -2", "MOD(wide,-1)", "wide % -1"} {
		_, err := c.integerExpression(expression)
		require.NoError(t, err, expression)
	}
}
