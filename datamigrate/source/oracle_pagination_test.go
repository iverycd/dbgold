package source

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func oraclePageTestReader(t *testing.T, version int) (*OracleReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return &OracleReader{db: db, owner: `Mi"xed`, version: version}, mock
}

func TestOracleVersionDetectionUsesCompatibleDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version any
		err     error
		want    int
	}{
		{name: "no dictionary permission", err: errors.New("ORA-00942: table or view does not exist"), want: 11},
		{name: "empty version", version: "", want: 11},
		{name: "unparseable version", version: "unknown", want: 11},
		{name: "null version", version: nil, want: 11},
		{name: "nonpositive version", version: "0.0.0", want: 11},
		{name: "negative version", version: "-1.0.0", want: 11},
		{name: "11g", version: "11.2.0.4.0", want: 11},
		{name: "12c", version: "12.1.0.2.0", want: 12},
		{name: "19c", version: " 19.0.0.0.0 ", want: 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := oraclePageTestReader(t, 0)
			query := mock.ExpectQuery(`SELECT VERSION FROM V$INSTANCE`)
			if tc.err != nil {
				query.WillReturnError(tc.err)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"VERSION"}).AddRow(tc.version)).RowsWillBeClosed()
			}
			require.Equal(t, tc.want, detectOracleMajorVersion(r.db))
		})
	}
}

func TestOracleReadPagePreservesSourceColumnsAndPagingOrder(t *testing.T) {
	for _, version := range []int{0, 11, 12, 19} {
		for _, hasPK := range []bool{false, true} {
			t.Run(fmt.Sprintf("version%d_pk%t", version, hasPK), func(t *testing.T) {
				r, mock := oraclePageTestReader(t, version)
				const table = `Ta"ble`
				names := []string{`Id"Key`, "RNCOLUMN", "DBGOLD_PAGE_ROWNUM", "DBGOLD_PAGE_ROWNUM_", "Payload"}
				var pks []string
				order := "ROWID"
				if hasPK {
					pks = []string{`Id"Key`, "RNCOLUMN"}
					order = `"Id""Key", "RNCOLUMN"`
				}
				base := `SELECT * FROM "Mi""xed"."Ta""ble" ORDER BY ` + order
				query := base + " OFFSET 10 ROWS FETCH NEXT 2 ROWS ONLY"
				if version < 12 {
					metadata := sqlmock.NewRows([]string{"COLUMN_NAME"})
					for _, name := range names {
						metadata.AddRow(name)
					}
					mock.ExpectQuery(`SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS WHERE OWNER = :1 AND TABLE_NAME = :2 ORDER BY COLUMN_ID`).WithArgs(r.owner, table).WillReturnRows(metadata).RowsWillBeClosed()
					query = `SELECT DBG_PAGE."Id""Key", DBG_PAGE."RNCOLUMN", DBG_PAGE."DBGOLD_PAGE_ROWNUM", DBG_PAGE."DBGOLD_PAGE_ROWNUM_", DBG_PAGE."Payload" FROM (SELECT DBG_INNER.*, ROWNUM "DBGOLD_PAGE_ROWNUM__" FROM (` + base + `) DBG_INNER WHERE ROWNUM <= 12) DBG_PAGE WHERE DBG_PAGE."DBGOLD_PAGE_ROWNUM__" > 10`
				}
				definitions := []*sqlmock.Column{
					sqlmock.NewColumn(names[0]).OfType("NUMBER", int64(0)),
					sqlmock.NewColumn(names[1]).OfType("VARCHAR2", ""),
					sqlmock.NewColumn(names[2]).OfType("NUMBER", int64(0)),
					sqlmock.NewColumn(names[3]).OfType("NUMBER", int64(0)),
					sqlmock.NewColumn(names[4]).OfType("BLOB", []byte{}),
				}
				mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(definitions...).AddRow(11, "real rownum column", 101, 102, []byte{0, 255})).RowsWillBeClosed()
				cols, types, rows, err := r.ReadPage(context.Background(), table, pks, 10, 2)
				require.NoError(t, err)
				require.Equal(t, names, cols)
				require.Equal(t, []string{"NUMBER", "VARCHAR2", "NUMBER", "NUMBER", "BLOB"}, types)
				require.Equal(t, [][]any{{int64(11), "real rownum column", int64(101), int64(102), []byte{0, 255}}}, rows)
				// The next request reuses source column metadata and returns no helper.
				emptyQuery := strings.ReplaceAll(query, "<= 12", "<= 14")
				emptyQuery = strings.ReplaceAll(emptyQuery, "> 10", "> 12")
				emptyQuery = strings.ReplaceAll(emptyQuery, "OFFSET 10", "OFFSET 12")
				mock.ExpectQuery(emptyQuery).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(definitions...)).RowsWillBeClosed()
				cols, types, rows, err = r.ReadPage(context.Background(), table, pks, 12, 2)
				require.NoError(t, err)
				require.Equal(t, names, cols)
				require.Len(t, types, len(names))
				require.Empty(t, rows)
			})
		}
	}
}

func TestOracleReadPageVersionPermissionFailureUsesLegacySQL(t *testing.T) {
	r, mock := oraclePageTestReader(t, 0)
	mock.ExpectQuery(`SELECT VERSION FROM V$INSTANCE`).WillReturnError(errors.New("ORA-00942"))
	r.version = detectOracleMajorVersion(r.db)
	mock.ExpectQuery(`SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS WHERE OWNER = :1 AND TABLE_NAME = :2 ORDER BY COLUMN_ID`).WithArgs(r.owner, "T").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME"}).AddRow("ID"))
	mock.ExpectQuery(`SELECT DBG_PAGE."ID" FROM (SELECT DBG_INNER.*, ROWNUM "DBGOLD_PAGE_ROWNUM" FROM (SELECT * FROM "Mi""xed"."T" ORDER BY "ID") DBG_INNER WHERE ROWNUM <= 10) DBG_PAGE WHERE DBG_PAGE."DBGOLD_PAGE_ROWNUM" > 0`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("ID").OfType("NUMBER", int64(0))).AddRow(1))
	_, _, rows, err := r.ReadPage(context.Background(), "T", []string{"ID"}, 0, 10)
	require.NoError(t, err)
	require.Equal(t, [][]any{{int64(1)}}, rows)
}

func TestOracleReadPageRejectsInvalidRange(t *testing.T) {
	r, _ := oraclePageTestReader(t, 11)
	for _, args := range [][2]int64{{-1, 10}, {0, 0}, {0, -1}, {math.MaxInt64, 1}} {
		_, _, _, err := r.ReadPage(context.Background(), "T", nil, args[0], args[1])
		require.ErrorContains(t, err, "无效分页范围")
	}
}

func TestOracleReadPageDoesNotRetryUnrelatedErrors(t *testing.T) {
	r, mock := oraclePageTestReader(t, 19)
	mock.ExpectQuery(`SELECT * FROM "Mi""xed"."T" ORDER BY ROWID OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY`).WillReturnError(errors.New("ORA-00942: table or view does not exist"))
	_, _, _, err := r.ReadPage(context.Background(), "T", nil, 0, 10)
	require.ErrorContains(t, err, "ORA-00942")
}

func TestOracleReadPageConcurrentLegacyPages(t *testing.T) {
	r, mock := oraclePageTestReader(t, 11)
	r.pageColumns.Store("T", []string{"ID"})
	mock.MatchExpectationsInOrder(false)
	for offset := range 8 {
		query := fmt.Sprintf(`SELECT DBG_PAGE."ID" FROM (SELECT DBG_INNER.*, ROWNUM "DBGOLD_PAGE_ROWNUM" FROM (SELECT * FROM "Mi""xed"."T" ORDER BY "ID") DBG_INNER WHERE ROWNUM <= %d) DBG_PAGE WHERE DBG_PAGE."DBGOLD_PAGE_ROWNUM" > %d`, offset+1, offset)
		mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("ID").OfType("NUMBER", int64(0))).AddRow(offset))
	}
	var workers sync.WaitGroup
	errors := make(chan error, 8)
	for offset := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _, rows, err := r.ReadPage(context.Background(), "T", []string{"ID"}, int64(offset), 1)
			if err == nil && (len(rows) != 1 || rows[0][0] != int64(offset)) {
				err = fmt.Errorf("unexpected page %d: %v", offset, rows)
			}
			errors <- err
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
}

func TestOracleForeignKeysPreserveReferencedOwner(t *testing.T) {
	r, mock := oraclePageTestReader(t, 19)
	// Match the projected owner explicitly; omitting it silently redirected
	// cross-schema references to the migration's target schema.
	mock.ExpectQuery(`SELECT c.CONSTRAINT_NAME, c.TABLE_NAME, cc.COLUMN_NAME, rc.OWNER AS REF_SCHEMA, rc.TABLE_NAME AS REF_TABLE, rcc.COLUMN_NAME AS REF_COLUMN, c.DELETE_RULE, 'NO ACTION' AS UPDATE_RULE FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME JOIN ALL_CONSTRAINTS rc ON c.R_OWNER = rc.OWNER AND c.R_CONSTRAINT_NAME = rc.CONSTRAINT_NAME JOIN ALL_CONS_COLUMNS rcc ON rc.OWNER = rcc.OWNER AND rc.CONSTRAINT_NAME = rcc.CONSTRAINT_NAME AND cc.POSITION = rcc.POSITION WHERE c.CONSTRAINT_TYPE = 'R' AND c.OWNER = :1 ORDER BY c.CONSTRAINT_NAME, cc.POSITION`).WithArgs(r.owner).WillReturnRows(sqlmock.NewRows([]string{"CONSTRAINT_NAME", "TABLE_NAME", "COLUMN_NAME", "REF_SCHEMA", "REF_TABLE", "REF_COLUMN", "DELETE_RULE", "UPDATE_RULE"}).AddRow("MixedFk", "Child", "ParentId", "OtherOwner", "Parent", "Id", "NO ACTION", "NO ACTION").AddRow("MixedFk", "Child", "ParentCode", "OtherOwner", "Parent", "Code", "NO ACTION", "NO ACTION"))
	fks, err := r.GetForeignKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, fks, 1)
	require.Equal(t, "OtherOwner", fks[0].RefSchema)
	require.Equal(t, "Child", fks[0].TableName)
	require.Equal(t, []string{"ParentId", "ParentCode"}, fks[0].Columns)
	require.Equal(t, []string{"Id", "Code"}, fks[0].RefColumns)
	require.Equal(t, r.owner, r.MigrationSchema())
}
