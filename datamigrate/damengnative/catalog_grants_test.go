package damengnative

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGrantCatalogIdentityRetainsMixedCase(t *testing.T) {
	for _, ddl := range []string{`GRANT SELECT ON MixedTable TO PUBLIC`, `GRANT SELECT ON "App"."MixedTable" TO PUBLIC`, `GRANT UPDATE("MixedColumn") ON TABLE MixedTable TO "MiXeDUser" WITH GRANT OPTION`} {
		bound, err := bindNativeGrantObject(ddl, "App", "MixedTable")
		require.NoError(t, err)
		require.Contains(t, bound, `ON `)
		require.Contains(t, bound, `"App"."MixedTable"`)
		mapped, _, err := RewriteSchema(bound, "App", "Target")
		require.NoError(t, err)
		require.Contains(t, mapped, `"Target"."MixedTable"`)
		if ddl[len(ddl)-6:] != "PUBLIC" {
			require.Contains(t, mapped, `"MixedColumn"`)
			require.Contains(t, mapped, `"MiXeDUser" WITH GRANT OPTION`)
		}
	}
	_, err := bindNativeGrantObject(`GRANT SELECT ON Other TO PUBLIC`, "App", "MixedTable")
	require.Error(t, err)
	_, err = bindNativeGrantObject(`GRANT SELECT ON "Outside"."MixedTable" TO PUBLIC`, "App", "MixedTable")
	require.Error(t, err)
}

func TestDamengPackageBodyUsesDocumentedPackageCompile(t *testing.T) {
	_, _, _, compile := ddlOptions(Object{Kind: "package_body", Schema: "App", Name: "MixedPkg"})
	require.Equal(t, `ALTER PACKAGE "App"."MixedPkg" COMPILE`, compile)
}

func TestNativeMetadataNewlineSeparatedColumnGrants(t *testing.T) {
	units, err := splitSimpleDDL("GRANT SELECT ON MixedTable TO PUBLIC\r\nGRANT UPDATE(MiXeDColumn) ON MixedTable TO UserName WITH GRANT OPTION\r\nGRANT REFERENCES(MiXeDColumn) ON MixedTable TO PUBLIC;")
	require.NoError(t, err)
	require.Len(t, units, 3)
	bound, err := bindNativeGrantObject(units[1], "App", "MixedTable")
	require.NoError(t, err)
	require.Equal(t, `GRANT UPDATE("MiXeDColumn") ON "App"."MixedTable" TO UserName WITH GRANT OPTION`, bound)
}
