package damengnative

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeclaredTableChildrenPreservesNativeNamesAndIgnoresData(t *testing.T) {
	objects := []Object{{ID: "table", Kind: "table", Schema: "Mixed", Name: "T", Units: []Unit{
		{SQL: `CREATE TABLE "Mixed"."T"("Note" VARCHAR(50) DEFAULT 'CONSTRAINT Fake', "(" INT, CONSTRAINT "ExactCase" CHECK("Note"<>'CREATE INDEX Bad'), CONSTRAINT UNQUOTED PRIMARY KEY("("))`},
		{SQL: `CREATE OR REPLACE UNIQUE INDEX "Mixed"."IndexCase" ON "Mixed"."T"("(")`},
	}}}
	children := DeclaredTableChildren(objects)
	require.Len(t, children, 3)
	require.Equal(t, []string{"ExactCase", "UNQUOTED", "IndexCase"}, []string{children[0].Name, children[1].Name, children[2].Name})
	for _, child := range children {
		require.Equal(t, "table", child.ParentID)
		require.Equal(t, "Mixed", child.Schema)
		require.True(t, child.Managed)
		require.False(t, child.Generated)
	}
}

func TestDeclaredTableChildrenIncludesNamedClusterIndex(t *testing.T) {
	children := DeclaredTableChildren([]Object{{ID: "table", Kind: "table", Schema: "Mixed", Name: "T", Units: []Unit{
		{SQL: `CREATE OR REPLACE CLUSTER INDEX "Mixed"."ClusterOwn" ON "Mixed"."T"("Id")`},
		{SQL: `CREATE NOT PARTIAL INDEX "Mixed"."PartialOwn" ON "Mixed"."T"("Id")`},
	}}})
	require.Len(t, children, 2)
	require.Equal(t, "ClusterOwn", children[0].Name)
	require.Equal(t, "PartialOwn", children[1].Name)
	for _, child := range children {
		require.Equal(t, "index", child.Kind)
		require.Equal(t, "table", child.ParentID)
	}
}
