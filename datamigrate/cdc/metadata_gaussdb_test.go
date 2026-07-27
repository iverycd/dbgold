package cdc

import (
	"strings"
	"testing"
)

func TestUniqueIndexQuerySupportsGaussDBCatalogSyntax(t *testing.T) {
	for name, rawQuery := range map[string]string{
		"single-table": loadPostgresUniqueColumnSetsSQL,
		"schema-batch": loadTargetUniqueIndexesSQL,
	} {
		query := strings.ToLower(rawQuery)
		if !strings.Contains(query, "any(i.indkey)") {
			t.Fatalf("%s unique-index query must join pg_attribute without a correlated FROM function", name)
		}
		for _, unsupported := range []string{"with ordinality", "cross join lateral", "generate_subscripts", "indnkeyatts"} {
			if strings.Contains(query, unsupported) {
				t.Fatalf("%s unique-index query contains GaussDB-incompatible syntax %q", name, unsupported)
			}
		}
	}
}
