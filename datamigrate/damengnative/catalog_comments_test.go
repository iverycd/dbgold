package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeCommentsIncludeColumnsAndSequences(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "MiXeD", definitions: true, snapshot: &Snapshot{}, index: map[string]int{}, byRaw: map[int64][]int{}}
	r.add(1, Object{Kind: "table", Schema: r.schema, Name: "Orders"})
	r.add(2, Object{Kind: "sequence", Schema: r.schema, Name: "OrderSeq"})
	mock.ExpectQuery(`SELECT TVNAME,CAST\(NULL AS VARCHAR\(128\)\),COMMENT\$ FROM SYS.SYSTABLECOMMENTS`).WithArgs("MiXeD", "MiXeD").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "text"}).AddRow("Orders", nil, "Table AbC").AddRow("Orders", "MiXeDColumn", "Column's AbC").AddRow("OrderSeq", nil, "Sequence AbC"))
	mock.ExpectQuery(`SELECT g.OBJID,COUNT\(\*\) FROM SYS.SYSGRANTS`).WithArgs("MiXeD").WillReturnRows(sqlmock.NewRows([]string{"id", "count"}))
	require.NoError(t, r.readCommentsAndGrants())
	var sql []string
	for _, object := range r.snapshot.Objects {
		if object.Kind == "comment" {
			sql = append(sql, object.Units[0].SQL)
		}
	}
	require.ElementsMatch(t, []string{`COMMENT ON TABLE "MiXeD"."Orders" IS 'Table AbC'`, `COMMENT ON COLUMN "MiXeD"."Orders"."MiXeDColumn" IS 'Column''s AbC'`, `COMMENT ON SEQUENCE "MiXeD"."OrderSeq" IS 'Sequence AbC'`}, sql)
	require.NoError(t, mock.ExpectationsWereMet())
}
