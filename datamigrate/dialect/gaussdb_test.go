package dialect

import (
	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGaussDBDDLQuotesDistributionAndGSIKeys(t *testing.T) {
	d := NewPostgres("gaussdb")
	info := &source.TableDDLInfo{TableName: `Pat"ients`, Columns: []source.ColumnInfo{{Name: `Patient"ID`, DataType: "int"}}}
	ss, err := d.CreateTableStatements(`My"Schema`, info, "mysql", TypeOpt{GaussDBDistribution: []string{`Patient"ID`}}, identity)
	require.NoError(t, err)
	require.Contains(t, ss[0].SQL, `"My""Schema"."Pat""ients"`)
	require.Contains(t, ss[1].SQL, `) DISTRIBUTE BY HASH ("Patient""ID");`)
	idx := source.IndexInfo{TableName: info.TableName, IndexName: `Auto"ID`, Columns: []string{`Auto"ID`}, IsUnique: true, GaussDB: &source.GaussDBIndexPlan{Base: source.GaussDBDistribution{Schema: `My"Schema`}, Global: true, Columns: []string{`Auto"ID`}}}
	require.Equal(t, `CREATE GLOBAL UNIQUE INDEX "Auto""ID" ON "My""Schema"."Pat""ients" USING ubtree ("Auto""ID") DISTRIBUTE BY HASH ("Auto""ID");`, d.IndexStatements(`My"Schema`, idx)[0].SQL)
	for _, product := range []string{"postgres", "highgo", "kingbase"} {
		other := NewPostgres(product)
		ss, err = other.CreateTableStatements("App", info, "mysql", TypeOpt{GaussDBDistribution: []string{"id"}}, identity)
		require.NoError(t, err)
		require.NotContains(t, JoinSQL(ss), "DISTRIBUTE")
		require.NotContains(t, JoinSQL(other.IndexStatements("App", idx)), "GLOBAL")
	}
}
