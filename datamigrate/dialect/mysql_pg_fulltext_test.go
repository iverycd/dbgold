package dialect

import (
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func TestMySQLPGFulltextDDLUsesMappedNamesAndOrderedNullSafeExpression(t *testing.T) {
	idx := source.IndexInfo{
		TableName: `Frame"Storage`, IndexName: "SourceName", TargetIndexName: `Target"Index`,
		Method: "FULLTEXT", PGFulltext: true, Columns: []string{`Client"Tag`, "ClientGuid"},
	}
	const want = `CREATE INDEX "Target""Index" ON "App""Schema"."Frame""Storage" USING gin (pg_catalog.to_tsvector('pg_catalog.simple'::pg_catalog.regconfig, COALESCE("Client""Tag"::pg_catalog.text, ''::pg_catalog.text) OPERATOR(pg_catalog.||) ' '::pg_catalog.text OPERATOR(pg_catalog.||) COALESCE("ClientGuid"::pg_catalog.text, ''::pg_catalog.text)) pg_catalog.tsvector_ops);`
	for _, product := range []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"} {
		t.Run(product, func(t *testing.T) {
			require.Equal(t, want, JoinSQL(NewPostgres(product).IndexStatements(`App"Schema`, idx)))
		})
	}
}
