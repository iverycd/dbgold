package handler

import (
	"testing"

	"dbgold/datamigrate/repair"
	"github.com/stretchr/testify/require"
)

func TestRepairDamengContextIndexScope(t *testing.T) {
	obj := repair.Object{Kind: "index", Schema: "APP", Table: "ARTICLES", TargetName: "ARTICLES_IDX_BODY"}
	generated := `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`
	for _, sql := range []string{
		generated,
		generated + ";",
		"-- migrated FULLTEXT\n/* original index */\n" + generated,
		`create context index "ARTICLES_IDX_BODY" on "APP"."ARTICLES" ("BODY") sync transaction`,
		`CREATE CONTEXT INDEX "APP"."ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`,
	} {
		units := []repair.Unit{{SQL: sql, OriginalSQL: generated, Selected: true}}
		require.NoError(t, validateRepairUnits(units), sql)
		require.NoError(t, validateRepairObjectScope(obj, units, "dameng"), sql)
	}

	for _, dbType := range []string{"oracle", "mysql", "postgres", "gaussdb", "sqlserver"} {
		t.Run(dbType, func(t *testing.T) {
			units := []repair.Unit{{SQL: generated, OriginalSQL: generated, Selected: true}}
			require.ErrorContains(t, validateRepairObjectScope(obj, units, dbType), "仅支持达梦")
		})
	}
}

func TestRepairDamengContextIndexRejectsScopeChanges(t *testing.T) {
	obj := repair.Object{Kind: "index", Schema: "APP", Table: "ARTICLES", TargetName: "ARTICLES_IDX_BODY"}
	generated := `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`
	for _, tc := range []struct{ name, sql string }{
		{"index schema", `CREATE CONTEXT INDEX "OTHER"."ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`},
		{"index name", `CREATE CONTEXT INDEX "WRONG" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`},
		{"table schema", `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "OTHER"."ARTICLES" ("BODY") SYNC TRANSACTION`},
		{"table name", `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."WRONG" ("BODY") SYNC TRANSACTION`},
		{"multiple statements", generated + `; CREATE INDEX "OTHER" ON "APP"."ARTICLES" ("BODY")`},
		{"comment table decoy", `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" /* ON "APP"."ARTICLES" */ ON "OTHER"."WRONG" ("BODY") SYNC TRANSACTION`},
		{"commented target", `CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON /* "APP"."ARTICLES" */ "OTHER"."WRONG" ("BODY") SYNC TRANSACTION`},
		{"commented head", `CREATE /* original */ CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`},
		{"executable comment", generated + ` /*! CREATE INDEX OTHER ON APP.WRONG(BODY) */`},
		{"wrong object type", `CREATE CONTEXT TABLE "ARTICLES_IDX_BODY" ("BODY" VARCHAR(20))`},
		{"wrong modifier", `CREATE UNIQUE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units := []repair.Unit{{SQL: tc.sql, OriginalSQL: generated, Selected: true}}
			require.Error(t, validateRepairObjectScope(obj, units, "dameng"))
		})
	}
}

func TestRepairContextIndexHeadPreservesIndexIdentity(t *testing.T) {
	kind, parts, tail, err := repairHead(`CREATE CONTEXT INDEX "ARTICLES_IDX_BODY" ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`)
	require.NoError(t, err)
	require.Equal(t, "index", kind)
	require.Equal(t, []string{"ARTICLES_IDX_BODY"}, parts)
	require.Equal(t, `ON "APP"."ARTICLES" ("BODY") SYNC TRANSACTION`, tail)
	for _, sql := range []string{"CREATE CONTEXT TABLE x (id INT)", "CREATE CONTEXT VIEW x AS SELECT 1", "CREATE UNIQUE CONTEXT INDEX x ON t(c)"} {
		kind, _, _, err = repairHead(sql)
		require.NoError(t, err)
		require.Empty(t, kind, sql)
	}
}
