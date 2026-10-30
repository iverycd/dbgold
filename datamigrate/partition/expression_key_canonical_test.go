package partition

import (
	"testing"

	"dbgold/datamigrate/source"
)

func TestKeyRoutingCanonicalNativeDeparse(t *testing.T) {
	pairs := [][2]string{
		{`CAST('4294967295' AS bigint)`, `'4294967295'::bigint`},
		{`CAST("Key" AS bigint)`, `("Key")::bigint`},
		{`pg_catalog.decode('00ff','hex')`, `decode('00ff'::text, 'hex'::text)`},
		{`CAST(pg_catalog.get_byte(pg_catalog.decode('00ff','hex'),CAST("Key" AS integer)) AS bigint)`, `(get_byte(decode('00ff'::text, 'hex'::text), ("Key")::integer))::bigint`},
		{`CASE WHEN "Key" IS NULL THEN CAST('0' AS bigint) ELSE pg_catalog.int8and(CAST("Key" AS bigint), CAST('255' AS bigint)) END`, `CASE WHEN ("Key" IS NULL) THEN '0'::bigint ELSE int8and(("Key")::bigint, '255'::bigint) END`},
		{`CASE WHEN pg_catalog.int8lt(CAST("Key" AS bigint),CAST('-128' AS bigint)) THEN CAST('-1' AS bigint) WHEN pg_catalog.int8gt(CAST("Key" AS bigint),CAST('127' AS bigint)) THEN CAST('-1' AS bigint) ELSE CAST('0' AS bigint) END`, `CASE WHEN int8lt("Key"::bigint, '-128'::bigint) THEN '-1'::bigint WHEN int8gt("Key"::bigint, '127'::bigint) THEN '-1'::bigint ELSE '0'::bigint END`},
	}
	for _, pair := range pairs {
		a, ok := canonicalRoutingExpression(pair[0])
		b, otherOK := canonicalRoutingExpression(pair[1])
		if !ok || !otherOK || a != b {
			t.Fatalf("native expression identity differs:\n%s\n%s\n%s\n%s", pair[0], pair[1], a, b)
		}
	}
}

func TestKeyRoutingCanonicalRetainsSemanticChanges(t *testing.T) {
	pairs := [][2]string{
		{`CAST("null" AS bigint)`, `NULL::bigint`},
		{`"true"`, `TRUE`},
		{`"false"`, `FALSE`},
		{`CAST("K" AS bigint)`, `CAST("K" AS integer)`},
		{`pg_catalog.int8xor("K",1)`, `public.int8xor("K",1)`},
		{`pg_catalog.int8xor("K",1)`, `"INT8XOR"("K",1)`},
		{`CASE WHEN "K" IS NULL THEN 0 ELSE 1 END`, `CASE WHEN "K" IS NOT NULL THEN 0 ELSE 1 END`},
		{`CASE WHEN int8lt("K",-128) THEN -1 ELSE 0 END`, `CASE WHEN int8lt("K",-129) THEN -1 ELSE 0 END`},
		{`decode('00ff','hex')`, `decode('00fe','hex')`},
		{`CAST('2147483648' AS integer)`, `2147483648`},
	}
	for _, pair := range pairs {
		if CanonicalKey(source.PartitionKey{Expression: pair[0]}) == CanonicalKey(source.PartitionKey{Expression: pair[1]}) {
			t.Fatalf("changed expression treated as identical: %s / %s", pair[0], pair[1])
		}
	}
}
