package cdc

import (
	"strings"
	"testing"
)

func TestUniqueIndexQuerySupportsGaussDBCatalogSyntax(t *testing.T) {
	query := strings.ToLower(loadPostgresUniqueColumnSetsSQL)
	if !strings.Contains(query, "any(i.indkey)") {
		t.Fatal("unique-index query must join pg_attribute without a correlated FROM function")
	}
	for _, unsupported := range []string{"with ordinality", "cross join lateral", "generate_subscripts", "indnkeyatts"} {
		if strings.Contains(query, unsupported) {
			t.Fatalf("unique-index query contains GaussDB-incompatible syntax %q", unsupported)
		}
	}
}
