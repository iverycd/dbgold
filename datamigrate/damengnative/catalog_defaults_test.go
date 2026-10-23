package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeCopyFreezesExactOnNullExpressionWithoutEvaluatingIt(t *testing.T) {
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "Mixed"}
	o := Object{Kind: "table", Units: []Unit{{SQL: `CREATE TABLE "Mixed"."T"("V" INT DEFAULT ON NULL "Mixed"."Seq".NEXTVAL,"Text" VARCHAR(80) DEFAULT 'DEFAULT ON NULL',"(" INT)`, Phase: "before_data"}}}
	table := Table{Name: "T", Columns: []Column{{Name: "V", Writable: true}, {Name: "Text", Writable: true}, {Name: "(", Writable: true}}}
	m.ExpectQuery(`SELECT COLUMN_NAME,DATA_DEFAULT`).WithArgs("Mixed", "T").WillReturnRows(sqlmock.NewRows([]string{"column", "default"}).AddRow("V", `"Mixed"."Seq".NEXTVAL`).AddRow("Text", `'DEFAULT ON NULL'`).AddRow("(", nil))
	require.NoError(t, r.freezeNativeCopyDefaults(o, &table, "T"))
	require.True(t, table.Columns[0].DefaultOnNull)
	require.Equal(t, `"Mixed"."Seq".NEXTVAL`, table.Columns[0].DefaultSQL)
	require.False(t, table.Columns[1].DefaultOnNull)
	require.False(t, table.Columns[2].DefaultOnNull)
	require.NoError(t, m.ExpectationsWereMet(), "preflight must never select NEXTVAL")
}
