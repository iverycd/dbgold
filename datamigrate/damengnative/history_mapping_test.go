package damengnative

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteMaterializedHistoryPreservesFrozenSourceAndLiteralContent(t *testing.T) {
	original := &MaterializedHistoryPlan{Version: 1, Schema: "MiX", Views: []MaterializedHistoryView{{Name: "Mv", SourceRefresh: "2026-10-22 12:34:56.123456", Query: `SELECT "MiX"."T"."Id",'MiX.T' FROM "MiX"."T" /* MiX.T */`, RowIDColumns: []int{0}, Columns: []Column{{Name: "Id", TypeSchema: "MiX", SQLType: `"MiX"."Ty"`}}, Buffer: MaterializedExchangeBuffer{Name: "Buf", CreateSQL: `CREATE TABLE "MiX"."Buf"("Id" "MiX"."Ty")`, Indexes: []string{`CREATE INDEX "MiX"."I" ON "MiX"."Buf"("Id")`}}}}, Logs: []MaterializedHistoryLog{{Table: "MLOG$_T", Columns: []Column{{Name: "Id", SQLType: `"MiX"."Ty"`, TypeSchema: "MiX"}}, Buffer: MaterializedExchangeBuffer{CreateSQL: `CREATE TABLE "MiX"."LogBuffer"("Id" INT)`}}}, RowIDMaps: []MaterializedRowIDMap{{BaseTable: "T", Table: "RidMap"}}}
	original.Views[0].Redefinition = &MaterializedRedefinitionPlan{WorkingTable: "Work", SwapName: "Swap", CreateSQL: `CREATE TABLE "MiX"."Work"("Id" "MiX"."Ty", "Text" VARCHAR(20) DEFAULT 'MiX.T')`}
	original.Views[0].EmptyTouch = &MaterializedEmptyTouch{Table: "T", DefinitionSQL: `CREATE TABLE "MiX"."T"("Id" INT)`, Columns: []MaterializedEmptyColumn{{Name: "Id", DefaultSQL: `"MiX"."Seq".NEXTVAL`}, {Name: "Text", DefaultSQL: `'MiX.T'`}}, Constraints: []MaterializedEmptyConstraint{{Table: "T", Name: "Fk", Columns: []string{"Id"}, DefinitionSQL: `ALTER TABLE "MiX"."T" ADD CONSTRAINT "Fk" FOREIGN KEY("Id") REFERENCES "MiX"."Parent"("Id")`}}, IdentityRestore: []string{`ALTER TABLE "MiX"."T" DROP IDENTITY`}}
	before, err := json.Marshal(original)
	require.NoError(t, err)
	mapped, err := RewriteMaterializedHistory(original, "MiX", `Target"Case`)
	require.NoError(t, err)
	require.Equal(t, `Target"Case`, mapped.Schema)
	require.Contains(t, mapped.Views[0].Query, `"Target""Case"."T"."Id"`)
	require.Contains(t, mapped.Views[0].Query, `'MiX.T'`)
	require.Contains(t, mapped.Views[0].Query, `/* MiX.T */`)
	require.Equal(t, original.Views[0].SourceRefresh, mapped.Views[0].SourceRefresh)
	require.Equal(t, `Target"Case`, mapped.Views[0].Columns[0].TypeSchema)
	require.Equal(t, `"Target""Case"."Ty"`, mapped.Logs[0].Columns[0].SQLType)
	require.Contains(t, mapped.Views[0].Buffer.Indexes[0], `"Target""Case"."Buf"`)
	require.Contains(t, mapped.Views[0].Redefinition.CreateSQL, `"Target""Case"."Work"`)
	require.Contains(t, mapped.Views[0].Redefinition.CreateSQL, `'MiX.T'`)
	require.Contains(t, mapped.Views[0].EmptyTouch.DefinitionSQL, `"Target""Case"."T"`)
	require.Equal(t, `"Target""Case"."Seq".NEXTVAL`, mapped.Views[0].EmptyTouch.Columns[0].DefaultSQL)
	require.Equal(t, `'MiX.T'`, mapped.Views[0].EmptyTouch.Columns[1].DefaultSQL)
	require.Equal(t, "T", mapped.Views[0].EmptyTouch.Constraints[0].Table)
	require.Contains(t, mapped.Views[0].EmptyTouch.Constraints[0].DefinitionSQL, `REFERENCES "Target""Case"."Parent"`)
	require.Contains(t, mapped.Views[0].EmptyTouch.IdentityRestore[0], `"Target""Case"."T"`)
	mapped.Views[0].RowIDColumns[0] = 7
	mapped.Views[0].Redefinition.WorkingTable = "changed"
	mapped.Views[0].EmptyTouch.Constraints[0].Columns[0] = "changed"
	mapped.Views[0].EmptyTouch.Columns[0].DefaultSQL = "changed"
	mapped.Views[0].EmptyTouch.IdentityRestore[0] = "changed"
	mapped.Views[0].Buffer.Indexes[0] = "changed"
	mapped.RowIDMaps[0].Table = "changed"
	after, err := json.Marshal(original)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "目标 Schema 映射不能修改源冻结计划")
	_, err = RewriteMaterializedHistory(original, "OTHER", "T")
	require.Error(t, err)
}
