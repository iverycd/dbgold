package source

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// Match the complete dictionary query: sqlmock cannot validate that a selected
// field actually belongs to its dictionary view. Prefix matches hid ORA-00904.
const oracleDMExpectedColumnSQL = `SELECT c.COLUMN_NAME, c.DATA_TYPE,
 NVL(c.CHAR_LENGTH,0), c.DATA_LENGTH, NVL(c.DATA_PRECISION,0), NVL(c.DATA_SCALE,0),
 c.NULLABLE, c.DATA_DEFAULT, NVL(c.CHAR_USED,'B'), c.COLUMN_ID, c.DATA_TYPE_OWNER, vc.VIRTUAL_COLUMN
 FROM ALL_TAB_COLUMNS c JOIN ALL_TAB_COLS vc
 ON vc.OWNER=c.OWNER AND vc.TABLE_NAME=c.TABLE_NAME AND vc.COLUMN_NAME=c.COLUMN_NAME
 WHERE c.OWNER=:1 AND c.TABLE_NAME=:2 ORDER BY c.COLUMN_ID`

func oracleDMTestReader(t *testing.T, version int) (*OracleDamengReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); db.Close() })
	return WrapOracleForDameng(&OracleReader{db: db, owner: "APP", version: version}), mock
}

func TestOracleDamengUnknownVersionUses11g(t *testing.T) {
	r, _ := oracleDMTestReader(t, 0)
	require.Equal(t, 11, r.version)
}

func TestOracleDamengColumnMetadataPreservesSource(t *testing.T) {
	r, mock := oracleDMTestReader(t, 19)
	rows := sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHAR_LENGTH", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "DATA_DEFAULT", "CHAR_USED", "COLUMN_ID", "DATA_TYPE_OWNER", "VIRTUAL_COLUMN"}).
		AddRow("MixedText", "VARCHAR2", 30, 120, 0, 0, "Y", "'KeepCase'", "C", 1, nil, "NO").
		AddRow("RawData", "RAW", 0, 32, 0, 0, "Y", nil, "B", 2, nil, "NO").
		AddRow("CreatedAt", "TIMESTAMP(9) WITH TIME ZONE", 0, 13, 0, 9, "Y", nil, "B", 3, nil, "NO").
		AddRow("MixedId", "NUMBER", 0, 22, 20, 0, "N", `"APP"."ISEQ$$_42".nextval`, "B", 4, nil, "NO").
		AddRow("Xml", "XMLTYPE", 0, 2000, 0, 0, "Y", nil, "B", 5, "SYS", "NO")
	mock.ExpectQuery("^"+regexp.QuoteMeta(oracleDMExpectedColumnSQL)+"$").WithArgs("APP", "MixedTable").WillReturnRows(rows).RowsWillBeClosed()
	mock.ExpectQuery("SELECT i.COLUMN_NAME, i.GENERATION_TYPE").WithArgs("APP", "MixedTable").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "GENERATION_TYPE", "DEFAULT_ON_NULL"}).AddRow("MixedId", "BY DEFAULT", "YES")).RowsWillBeClosed()
	info, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
	require.NoError(t, err)
	require.Equal(t, "MixedText", info.Columns[0].Name)
	require.Equal(t, int64(30), info.Columns[0].Length)
	require.Equal(t, int64(120), info.Columns[0].DataLength)
	require.Equal(t, "C", info.Columns[0].CharUsed)
	require.Equal(t, "'KeepCase'", *info.Columns[0].Default)
	require.Equal(t, int64(32), info.Columns[1].Length)
	require.Equal(t, int64(9), info.Columns[2].DateTimePrecision)
	require.Equal(t, "BY DEFAULT ON NULL", info.Columns[3].Identity)
	require.Nil(t, info.Columns[3].Default)
	require.Equal(t, "XMLTYPE", info.Columns[4].DataType)
	require.Equal(t, "SYS", info.Columns[4].TypeSchema)
	require.Equal(t, "user_defined", info.Columns[4].TypeKind)
	require.Equal(t, "SYS.XMLTYPE", OracleDamengSourceType(info.Columns[4]))
	_, err = r.GetTableDDLInfo(context.Background(), "MixedTable")
	require.NoError(t, err) // projected pages reuse the immutable metadata.
}

func TestOracleDameng11gVirtualColumnMetadataUsesDictionaryJoin(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	rows := sqlmock.NewRows([]string{"COLUMN_NAME", "DATA_TYPE", "CHAR_LENGTH", "DATA_LENGTH", "DATA_PRECISION", "DATA_SCALE", "NULLABLE", "DATA_DEFAULT", "CHAR_USED", "COLUMN_ID", "DATA_TYPE_OWNER", "VIRTUAL_COLUMN"}).
		AddRow("MixedId", "NUMBER", 0, 22, 12, 0, "N", nil, "B", 1, nil, "NO").
		AddRow("MixedComputed", "NUMBER", 0, 22, 12, 0, "Y", `"MixedId"+1`, "B", 2, nil, "YES")
	mock.ExpectQuery("^"+regexp.QuoteMeta(oracleDMExpectedColumnSQL)+"$").WithArgs("APP", "MixedTable").WillReturnRows(rows).RowsWillBeClosed()
	info, err := r.GetTableDDLInfo(context.Background(), "MixedTable")
	require.NoError(t, err)
	require.Equal(t, "MixedId", info.Columns[0].Name)
	require.Empty(t, info.Columns[0].Generated)
	require.Equal(t, "MixedComputed", info.Columns[1].Name)
	require.Equal(t, "virtual", info.Columns[1].Generated)
	require.Equal(t, `"MixedId"+1`, *info.Columns[1].Default)
	_, err = r.GetTableDDLInfo(context.Background(), "MixedTable")
	require.NoError(t, err) // No identity query or repeated metadata query on 11g.
	_, _, _, err = r.ReadPage(context.Background(), "MixedTable", []string{"MixedId"}, 0, 10)
	require.ErrorContains(t, err, "MixedComputed")
	require.ErrorContains(t, err, "虚拟列")
}

func TestOracleDamengPagingPreservesBytesAndStableTypes(t *testing.T) {
	for _, version := range []int{11, 19} {
		t.Run(string(rune('A'+version)), func(t *testing.T) {
			r, mock := oracleDMTestReader(t, version)
			r.metadata[`Mixed"Table`] = &TableDDLInfo{TableName: `Mixed"Table`, Columns: []ColumnInfo{
				{Name: "Text", DataType: "VARCHAR2"}, {Name: "CreatedAt", DataType: "DATE"},
				{Name: "ZonedAt", DataType: "TIMESTAMP(9) WITH LOCAL TIME ZONE"},
				{Name: "Payload", DataType: "BLOB"}, {Name: "DBGOLD_PAGE_ROWNUM", DataType: "NUMBER"},
			}}
			query := `SELECT .*TO_CHAR\("CreatedAt", 'SYYYY-MM-DD HH24:MI:SS'.*AT TIME ZONE '\+00:00'.*FROM "APP"\."Mixed""Table" DBG_SOURCE ORDER BY "Text"`
			if version >= 12 {
				query += ` OFFSET 0 ROWS FETCH NEXT 2 ROWS ONLY`
			} else {
				query += `.*ROWNUM <= 2.*DBG_PAGE\."DBGOLD_PAGE_ROWNUM_" > 0`
			}
			mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRows([]string{"Text", "CreatedAt", "ZonedAt", "Payload", "DBGOLD_PAGE_ROWNUM"}).AddRow([]byte("a\x00b"), "2024-02-29 23:59:58", "2024-02-29 12:00:00.123456789 +00:00", []byte{}, "9007199254740993"))
			cols, types, rows, err := r.ReadPage(context.Background(), `Mixed"Table`, []string{"Text"}, 0, 2)
			require.NoError(t, err)
			require.Len(t, cols, 5)
			require.Equal(t, []string{"VARCHAR2", "DATE", "TIMESTAMP(9) WITH LOCAL TIME ZONE", "BLOB", "NUMBER"}, types)
			require.Equal(t, []byte("a\x00b"), rows[0][0])
			require.NotNil(t, rows[0][3])
			require.Equal(t, "9007199254740993", rows[0][4])
		})
	}
}

func TestOracleDamengSimpleTriggerGrammar(t *testing.T) {
	for _, tc := range []struct {
		body, when, column, seq string
		onNull, valid           bool
	}{
		{`BEGIN SELECT "SeqMixed".NEXTVAL INTO :NEW."MixedId" FROM DUAL; END;`, "", "MixedId", "SeqMixed", false, true},
		{`BEGIN IF :NEW.Id IS NULL THEN :NEW.Id := APP.SeqId.NEXTVAL; END IF; END;`, "", "ID", "SEQID", true, true},
		{`BEGIN /* harmless */ :NEW.Id := SeqId.NEXTVAL; END;`, "(NEW.Id IS NULL)", "ID", "SEQID", true, true},
		{`BEGIN SELECT SeqId.NEXTVAL INTO:NEW.Id FROM DUAL; END;`, "", "ID", "SEQID", false, true},
		{`BEGIN :NEW.Id := SeqId.NEXTVAL; DELETE FROM Audit; END;`, "", "", "", false, false},
		{`BEGIN IF :NEW.Other IS NULL THEN :NEW.Id := SeqId.NEXTVAL; END IF; END;`, "", "", "", false, false},
		{`BEGIN :NEW.Id := SeqId.NEXTVAL; END;`, "NEW.Id > 0", "", "", false, false},
		{`BEGIN :NEW.Id := SeqId.NEXTVAL; EXCEPTION WHEN OTHERS THEN NULL; END;`, "", "", "", false, false},
	} {
		binding, owner, seq, err := parseOracleDMTrigger(tc.body, tc.when, "APP", "TRG")
		if !tc.valid {
			require.Error(t, err, tc.body)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.column, binding.ColumnName)
		require.Equal(t, tc.seq, seq)
		require.Equal(t, "APP", owner)
		require.Equal(t, tc.onNull, binding.OnlyWhenNull)
	}
}

func TestOracleDamengSequencesDeduplicateAndScope(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default"}).AddRow("Mixed", "Text", "'NEXTVAL'"))
	mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}).
		AddRow(nil, "SchemaTrigger", "AFTER EVENT", "LOGON", `BEGIN someSeq.NEXTVAL; END;`, nil).
		AddRow("Mixed", "MixedTrigger", "BEFORE EACH ROW", "INSERT", `BEGIN IF :NEW."MixedId" IS NULL THEN :NEW."MixedId" := "MixedSeq".NEXTVAL; END IF; END;`, nil).
		AddRow("Other", "OtherTrigger", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW."OtherId" := "MixedSeq".NEXTVAL; END;`, nil))
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "MixedSeq").WillReturnRows(sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"}).AddRow("101", "5", "1", "9999999999999999999999999999", "20", "N", "N"))
	values, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Len(t, values[0].Native.Bindings, 2)
	selected := FilterNativeSequences(values, map[string]bool{"Mixed": true})
	require.Len(t, selected, 1)
	require.Len(t, selected[0].Native.Bindings, 1)
	require.Equal(t, "MixedSeq", selected[0].Native.Name)
	require.Equal(t, "MixedId", selected[0].Native.Bindings[0].ColumnName)
	require.Len(t, values[0].Native.Bindings, 2)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT TO_CHAR(MAX("MixedId"),'TM9','NLS_NUMERIC_CHARACTERS=''.,''') FROM "APP"."Mixed"`)).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow("108"))
	prepared, err := r.PrepareNativeSequence(context.Background(), selected[0])
	require.NoError(t, err)
	require.Equal(t, "111", prepared.StartValueText)
	require.Equal(t, "101", values[0].StartValueText)
}

func TestOracleDamengIdentityModesAreTableScoped(t *testing.T) {
	r, mock := oracleDMTestReader(t, 19)
	mock.ExpectQuery("SELECT i.TABLE_NAME,i.COLUMN_NAME,i.GENERATION_TYPE").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "col", "generation", "seq", "onNull"}).
		AddRow("Good", "Id", "BY DEFAULT", "ISEQ$$_1", "YES").AddRow("Unselected", "Id", "ALWAYS", "ISEQ$$_2", "NO"))
	mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "col", "default"}).AddRow("Good", "Id", `"APP"."ISEQ$$_1".nextval`).AddRow("Defaults", "Id", "SomeSeq.NEXTVAL"))
	mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}))
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "ISEQ$$_1").WillReturnRows(sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "order"}).AddRow("42", "1", "1", "9999999999999999999999999999", "20", "N", "N"))
	values, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 3)
	require.NotEmpty(t, values[1].Unsupported)
	require.NotEmpty(t, values[2].Unsupported)
	selected := FilterNativeSequences(values, map[string]bool{"Good": true})
	require.Len(t, selected, 1)
	require.Empty(t, selected[0].Unsupported)
	require.Equal(t, "identity", selected[0].Native.Bindings[0].Kind)
}

func TestOracleDamengSequenceBoundsExactAndDescending(t *testing.T) {
	for _, tc := range []struct{ start, step, bound, want string }{
		{"90071992547409931000", "3", "90071992547409931002", "90071992547409931003"},
		{"10", "-5", "-1", "-5"},
	} {
		r, mock := oracleDMTestReader(t, 11)
		agg := "MAX"
		if strings.HasPrefix(tc.step, "-") {
			agg = "MIN"
		}
		mock.ExpectQuery("SELECT TO_CHAR\\(" + agg).WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(tc.bound))
		s := SequenceInfo{StartValueText: tc.start, Native: &NativeSequenceInfo{Name: "S", IncrementBy: tc.step, MinValue: "-9999999999999999999999999999", MaxValue: "9999999999999999999999999999", Bindings: []SequenceBinding{{TableName: "T", ColumnName: "C"}}}}
		result, err := r.PrepareNativeSequence(context.Background(), s)
		require.NoError(t, err)
		require.Equal(t, tc.want, result.StartValueText)
	}
}

func TestOracleDamengSpecialIndexesAndForeignSchema(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	mock.ExpectQuery("SELECT i.TABLE_NAME, i.INDEX_NAME").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"table", "index", "column", "unique", "desc", "kind", "status"}).
		AddRow("Mixed", "Ordinary", "MixedColumn", "NONUNIQUE", "DESC", "NORMAL", "VALID").
		AddRow("Mixed", "Expression", "SYS_NC00001$", "NONUNIQUE", "ASC", "FUNCTION-BASED NORMAL", "VALID"))
	indexes, err := r.GetIndexes(context.Background())
	require.NoError(t, err)
	require.Len(t, indexes, 2)
	require.Equal(t, "MixedColumn", indexes[0].Columns[0])
	require.Equal(t, []bool{true}, indexes[0].Descending)
	require.Empty(t, indexes[0].Unsupported)
	require.NotEmpty(t, indexes[1].Unsupported)
	mock.ExpectQuery("SELECT c.CONSTRAINT_NAME,c.TABLE_NAME").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"name", "table", "column", "refTable", "refColumn", "delete", "owner", "status", "validated", "deferrable", "deferred"}).
		AddRow("MixedFK", "Mixed", "ParentId", "Parent", "Id", "CASCADE", "OtherOwner", "ENABLED", "VALIDATED", "NOT DEFERRABLE", "IMMEDIATE"))
	fks, err := r.GetForeignKeys(context.Background())
	require.NoError(t, err)
	require.Equal(t, "OtherOwner", fks[0].RefSchema)
	require.Equal(t, "ParentId", fks[0].Columns[0])
	require.NotEmpty(t, fks[0].Unsupported)
}

func TestOracleDamengCycleAndBoundsRejectBeforeReadingData(t *testing.T) {
	r, _ := oracleDMTestReader(t, 11)
	_, err := r.PrepareNativeSequence(context.Background(), SequenceInfo{StartValueText: "1", Native: &NativeSequenceInfo{IncrementBy: "1", Cycle: true}})
	require.ErrorContains(t, err, "循环")
}

func TestOracleDamengViewsBypassPostgresRewriting(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	raw := `SELECT NVL("MixedColumn",'KeepCase') AS "Label", ROWID AS "RowId" FROM "APP"."MixedTable" WHERE "Tag"='UPPER'; -- KeepComment`
	for range 2 {
		mock.ExpectQuery("SELECT VIEW_NAME,TEXT FROM ALL_VIEWS").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"name", "text"}).AddRow("MixedView", raw)).RowsWillBeClosed()
		mock.ExpectQuery("SELECT COLUMN_NAME,DATA_TYPE,NVL").WithArgs("APP", "MixedView").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "scale"}).AddRow("Label", "VARCHAR2", 0).AddRow("RowId", "ROWID", 0)).RowsWillBeClosed()
	}
	views, err := r.GetViews(context.Background())
	require.NoError(t, err)
	require.Equal(t, "MixedView", views[0].ViewName)
	require.Equal(t, raw, views[0].Definition)
	views, err = r.GetViewsForTarget(context.Background(), "dameng")
	require.NoError(t, err)
	require.Equal(t, raw, views[0].Definition)
}

func TestOracleDamengViewColumnHeadersReadAfterClosingViewRows(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	r.db.SetMaxOpenConns(1)
	raw := `SELECT "Id", 'KeepCase' AS "ProjectionAlias" FROM "APP"."MixedTable"`
	mock.ExpectQuery("SELECT VIEW_NAME,TEXT FROM ALL_VIEWS").WithArgs("APP").WillReturnRows(sqlmock.NewRows([]string{"name", "text"}).AddRow("MixedView", raw)).RowsWillBeClosed()
	mock.ExpectQuery("SELECT COLUMN_NAME,DATA_TYPE,NVL").WithArgs("APP", "MixedView").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "scale"}).AddRow("custom_col", "NUMBER", 0).AddRow("OtherName", "TIMESTAMP(9) WITH TIME ZONE", 9)).RowsWillBeClosed()
	views, err := r.GetViews(context.Background())
	require.NoError(t, err)
	require.Equal(t, raw, views[0].Definition)
	require.Equal(t, []ColumnInfo{{Name: "custom_col", DataType: "NUMBER"}, {Name: "OtherName", DataType: "TIMESTAMP(9) WITH TIME ZONE", Scale: 9, DateTimePrecision: 9}}, views[0].Columns)
}

func TestOracleDamengViewMetadataHonorsCancellation(t *testing.T) {
	r, _ := oracleDMTestReader(t, 11)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.GetViews(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestOracleDamengSpecialSourceProjections(t *testing.T) {
	for _, tc := range []struct {
		column ColumnInfo
		want   string
	}{
		{ColumnInfo{Name: "RowId", DataType: "ROWID"}, `ROWIDTOCHAR("RowId") AS "RowId"`},
		{ColumnInfo{Name: "Universal", DataType: "UROWID"}, `CAST("Universal" AS VARCHAR2(4000 BYTE)) AS "Universal"`},
		{ColumnInfo{Name: "Xml", DataType: "XMLTYPE", TypeSchema: "SYS"}, `XMLSERIALIZE(CONTENT "Xml" AS CLOB) AS "Xml"`},
		{ColumnInfo{Name: "External", DataType: "BFILE"}, `"External" AS "External"`},
		{ColumnInfo{Name: "Exact", DataType: "REAL"}, `TO_CHAR("Exact", 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''') AS "Exact"`},
		{ColumnInfo{Name: "Geo", DataType: "SDO_GEOMETRY", TypeSchema: "MDSYS"}, `CASE WHEN "Geo" IS NULL THEN NULL ELSE 'SRID=' || NVL(TO_CHAR(DBG_SOURCE."Geo".SDO_SRID, 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,'''), 'NULL') || ';' || MDSYS.SDO_UTIL.TO_WKTGEOMETRY("Geo") END AS "Geo"`},
		{ColumnInfo{Name: `Mixed"Geo`, DataType: "ST_GEOMETRY", TypeSchema: "SDE"}, `CASE WHEN "Mixed""Geo" IS NULL THEN NULL ELSE 'SRID=' || NVL(TO_CHAR(SDE.ST_SRID("Mixed""Geo"), 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,'''), 'NULL') || ';' || SDE.ST_ASTEXT("Mixed""Geo") END AS "Mixed""Geo"`},
		{ColumnInfo{Name: "Point", DataType: "ST_POINT", TypeSchema: "SYSGEO"}, `CASE WHEN "Point" IS NULL THEN NULL ELSE 'SRID=' || NVL(TO_CHAR(SYSGEO.ST_SRID("Point"), 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,'''), 'NULL') || ';' || SYSGEO.ST_ASTEXT("Point") END AS "Point"`},
	} {
		t.Run(tc.column.Name, func(t *testing.T) {
			require.Equal(t, tc.want, oracleDamengProjection(tc.column))
		})
	}
}

func TestOracleDamengSpecialPagingRetainsOwnerAndSerializedValues(t *testing.T) {
	r, mock := oracleDMTestReader(t, 19)
	info := &TableDDLInfo{TableName: "Objects", Columns: []ColumnInfo{
		{Name: "RowId", DataType: "ROWID"},
		{Name: "Universal", DataType: "UROWID"},
		{Name: "Xml", DataType: "XMLTYPE", TypeSchema: "SYS", TypeKind: "user_defined"},
		{Name: "Geo", DataType: "SDO_GEOMETRY", TypeSchema: "MDSYS", TypeKind: "user_defined"},
	}}
	r.metadata[info.TableName] = info
	query := `SELECT ROWIDTOCHAR("RowId") AS "RowId", CAST("Universal" AS VARCHAR2(4000 BYTE)) AS "Universal", XMLSERIALIZE(CONTENT "Xml" AS CLOB) AS "Xml", CASE WHEN "Geo" IS NULL THEN NULL ELSE 'SRID=' || NVL(TO_CHAR(DBG_SOURCE."Geo".SDO_SRID, 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,'''), 'NULL') || ';' || MDSYS.SDO_UTIL.TO_WKTGEOMETRY("Geo") END AS "Geo" FROM "APP"."Objects" DBG_SOURCE ORDER BY "RowId" OFFSET 0 ROWS FETCH NEXT 2 ROWS ONLY`
	mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$").WillReturnRows(sqlmock.NewRows([]string{"RowId", "Universal", "Xml", "Geo"}).
		AddRow("AAAFfIAAFAAAABSAAb", "*BAAAtAkCwQIBeP4", "<Tag>KeepCase</Tag>", "SRID=4326;POINT (1 2)").
		AddRow(nil, nil, nil, nil)).RowsWillBeClosed()
	cols, types, rows, err := r.ReadPage(context.Background(), "Objects", []string{"RowId"}, 0, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"RowId", "Universal", "Xml", "Geo"}, cols)
	require.Equal(t, []string{"ROWID", "UROWID", "SYS.XMLTYPE", "MDSYS.SDO_GEOMETRY"}, types)
	require.Equal(t, "<Tag>KeepCase</Tag>", rows[0][2])
	require.Equal(t, "SRID=4326;POINT (1 2)", rows[0][3])
	require.Equal(t, []interface{}{nil, nil, nil, nil}, rows[1])
}

func TestOracleDamengSourceSerializerProbeReadsNoRows(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	info := &TableDDLInfo{TableName: `Mixed"Objects`, Columns: []ColumnInfo{
		{Name: "Text", DataType: "VARCHAR2"},
		{Name: "Xml", DataType: "XMLTYPE", TypeSchema: "SYS", TypeKind: "user_defined"},
		{Name: "Geo", DataType: "ST_GEOMETRY", TypeSchema: "SDE", TypeKind: "user_defined"},
	}}
	for _, c := range info.Columns[1:] {
		query := "SELECT " + oracleDamengProjection(c) + ` FROM "APP"."Mixed""Objects" DBG_SOURCE WHERE 1=0`
		mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$").WillReturnRows(sqlmock.NewRows([]string{c.Name})).RowsWillBeClosed()
	}
	require.NoError(t, r.ValidateOracleDamengSource(context.Background(), []*TableDDLInfo{nil, info}))
	// Ordinary source types need no serializer capability query or special grants.
	require.NoError(t, r.ValidateOracleDamengSource(context.Background(), []*TableDDLInfo{{TableName: "Ordinary", Columns: info.Columns[:1]}}))
}

func TestOracleDamengSourceSerializerProbeExplainsMissingCapability(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	c := ColumnInfo{Name: "Geo", DataType: "ST_GEOMETRY", TypeSchema: "SDE", TypeKind: "user_defined"}
	query := "SELECT " + oracleDamengProjection(c) + ` FROM "APP"."Mixed" DBG_SOURCE WHERE 1=0`
	missing := errors.New("ORA-00904: ST_ASTEXT invalid identifier")
	mock.ExpectQuery("^" + regexp.QuoteMeta(query) + "$").WillReturnError(missing)
	err := r.ValidateOracleDamengSource(context.Background(), []*TableDDLInfo{{TableName: "Mixed", Columns: []ColumnInfo{c}}})
	require.ErrorIs(t, err, missing)
	require.ErrorContains(t, err, "Mixed")
	require.ErrorContains(t, err, "Geo")
	require.ErrorContains(t, err, "SDE.ST_GEOMETRY")
	c.DataType, c.TypeSchema = "ST_CURVE", "SYSGEO"
	err = r.ValidateOracleDamengSource(context.Background(), []*TableDDLInfo{{TableName: "Mixed", Columns: []ColumnInfo{c}}})
	require.ErrorContains(t, err, "二维空间转换器")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, r.ValidateOracleDamengSource(ctx, nil), context.Canceled)
}
