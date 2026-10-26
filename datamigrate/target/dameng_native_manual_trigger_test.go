package target

import (
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestNativeExpectedManualTriggerRetainsAuthoredTargetSchemaReferences(t *testing.T) {
	ddl := `CREATE TRIGGER "Dst"."AuditTrigger" AFTER DELETE ON "Dst"."Orders" FOR EACH ROW BEGIN INSERT INTO "Src"."Audit" VALUES ('Src.Orders'); END;`
	unit := damengnative.Unit{SQL: ddl, Phase: "after_data"}
	mapped, _, err := nativeExpectedUnit(unit, damengnative.Object{Kind: "trigger", Schema: "Dst"}, "Src", "Dst")
	require.NoError(t, err)
	require.Equal(t, ddl, mapped.SQL, "a target-authored trigger must not rewrite an intentional cross-schema reference")
	for _, owner := range []string{"Src", ""} {
		source := damengnative.Unit{SQL: `CREATE TRIGGER "Src"."AuditTrigger" AFTER DELETE ON "Src"."Orders" BEGIN NULL; END;`, Phase: "after_data"}
		mapped, _, err = nativeExpectedUnit(source, damengnative.Object{Kind: "trigger", Schema: owner}, "Src", "Dst")
		require.NoError(t, err)
		require.Equal(t, `CREATE TRIGGER "Dst"."AuditTrigger" AFTER DELETE ON "Dst"."Orders" BEGIN NULL; END;`, mapped.SQL)
	}
}

func TestNativeManualTriggerDefinitionProofAllowsCreateOrReplaceHeaderOnly(t *testing.T) {
	create := `CREATE TRIGGER "Dst"."AuditTrigger" AFTER DELETE ON "Dst"."Orders" FOR EACH ROW BEGIN INSERT INTO "Dst"."Audit" VALUES ('MiXeD'); END;`
	replace := `CREATE OR REPLACE TRIGGER "Dst"."AuditTrigger" AFTER DELETE ON "Dst"."Orders" FOR EACH ROW BEGIN INSERT INTO "Dst"."Audit" VALUES ('MiXeD'); END;`
	require.True(t, nativeDefinitionsEqual(create, replace))
	require.False(t, nativeDefinitionsEqual(create, `CREATE OR REPLACE TRIGGER "Dst"."AuditTrigger" AFTER DELETE ON "Dst"."Orders" FOR EACH ROW BEGIN INSERT INTO "Dst"."Audit" VALUES ('mixed'); END;`))
	require.False(t, nativeDefinitionsEqual(create, `CREATE OR REPLACE TRIGGER "Dst"."audittrigger" AFTER DELETE ON "Dst"."Orders" FOR EACH ROW BEGIN INSERT INTO "Dst"."Audit" VALUES ('MiXeD'); END;`))
}
