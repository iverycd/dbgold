package source

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func mysqlAutoReader(t *testing.T) (*OracleMySQLReader, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); db.Close() })
	return WrapOracleForMySQL(&OracleReader{db: db, owner: "APP", version: 19}), m
}
func mysqlAutoTriggerRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"TABLE_NAME", "TRIGGER_NAME", "TRIGGER_TYPE", "TRIGGERING_EVENT", "TRIGGER_BODY", "WHEN_CLAUSE", "STATUS", "OWNER"})
}

func TestOracleMySQLAutoInventorySharedOriginalNamesAndCache(t *testing.T) {
	r, m := mysqlAutoReader(t)
	rows := mysqlAutoTriggerRows().AddRow("MixedTable", "TG1", "BEFORE EACH ROW", "INSERT", `BEGIN SELECT "SeqOwner"."SharedSeq".NEXTVAL INTO :NEW."Id" FROM DUAL;end;`, nil, "ENABLED", "APP").AddRow("OtherTable", "TG2", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW."Id" := "SeqOwner"."SharedSeq".NEXTVAL; END;`, nil, "ENABLED", "APP")
	m.ExpectQuery(regexp.QuoteMeta(oracleMySQLTriggersSQL)).WithArgs("APP").WillReturnRows(rows).RowsWillBeClosed()
	m.ExpectQuery(regexp.QuoteMeta(oracleMySQLSequenceSQL)).WithArgs("SeqOwner", "SharedSeq").WillReturnRows(sqlmock.NewRows([]string{"last", "step", "cycle"}).AddRow("9007199254740993", "1", "N"))
	bindings, err := r.GetOracleMySQLAutoIncrements(context.Background())
	require.NoError(t, err)
	require.Len(t, bindings, 2)
	require.True(t, bindings[0].SharedSequence)
	require.True(t, bindings[1].SharedSequence)
	require.Equal(t, "MixedTable", bindings[0].TableName)
	require.Equal(t, "Id", bindings[0].ColumnName)
	require.Equal(t, "9007199254740993", bindings[0].StartValueText)
	bindings[0].ColumnName = "changed"
	seqs, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Id", seqs[0].ColumnName)
	require.Empty(t, seqs[0].Unsupported)
}

func TestOracleMySQLAutoTriggerGrammarAndRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body, when, kind, event, status, step, cycle string
		fail, disabled                                     bool
	}{
		{name: "screenshot", body: "BEGIN SELECT HIBERNATE_SEQUENCE.NEXTVAL INTO :NEW.Row_ID FROM DUAL; end;"},
		{name: "assignment", body: "BEGIN :NEW.ID:=APP.SEQ.NEXTVAL;END;"},
		{name: "null guard", body: "BEGIN IF :NEW.ID IS NULL THEN :NEW.ID:=SEQ.NEXTVAL; END IF; END;"},
		{name: "when", body: "BEGIN SELECT SEQ.NEXTVAL INTO:NEW.ID FROM DUAL;END;", when: "NEW.ID IS NULL"},
		{name: "comments", body: "/*head*/ BEGIN -- comment\n :NEW.ID:=SEQ.NEXTVAL; /* tail */ END;"},
		{name: "business logic", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL; :NEW.X:=1; END;", fail: true},
		{name: "multiple columns", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL; :NEW.OTHER:=SEQ.NEXTVAL;END;", fail: true},
		{name: "update event", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL;END;", event: "INSERT OR UPDATE", fail: true},
		{name: "after", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL;END;", kind: "AFTER EACH ROW", fail: true},
		{name: "disabled", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL;END;", status: "DISABLED", disabled: true},
		{name: "descending", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL;END;", step: "-1", fail: true},
		{name: "cycle", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL;END;", cycle: "Y", fail: true},
		{name: "stride", body: "BEGIN :NEW.ID:=SEQ.NEXTVAL;END;", step: "2", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, m := mysqlAutoReader(t)
			if tc.kind == "" {
				tc.kind = "BEFORE EACH ROW"
			}
			if tc.event == "" {
				tc.event = "INSERT"
			}
			if tc.status == "" {
				tc.status = "ENABLED"
			}
			if tc.step == "" {
				tc.step = "1"
			}
			if tc.cycle == "" {
				tc.cycle = "N"
			}
			m.ExpectQuery("FROM ALL_TRIGGERS").WillReturnRows(mysqlAutoTriggerRows().AddRow("T", "TG", tc.kind, tc.event, tc.body, tc.when, tc.status, "APP"))
			_, _, _, parseErr := parseOracleDMTrigger(tc.body, tc.when, "APP", "TG")
			if !tc.disabled && parseErr == nil && tc.kind == "BEFORE EACH ROW" && tc.event == "INSERT" {
				m.ExpectQuery("FROM ALL_SEQUENCES").WillReturnRows(sqlmock.NewRows([]string{"last", "step", "cycle"}).AddRow("100", tc.step, tc.cycle))
			}
			values, err := r.GetOracleMySQLAutoIncrements(context.Background())
			require.NoError(t, err)
			require.Len(t, values, 1)
			require.Equal(t, tc.fail, values[0].Error != "")
			require.Equal(t, tc.disabled, values[0].Disabled)
		})
	}
}

func TestOracleMySQLAutoMetadataFailureIsNotEmptySuccess(t *testing.T) {
	r, m := mysqlAutoReader(t)
	m.ExpectQuery("FROM ALL_TRIGGERS").WillReturnError(errors.New("permission denied"))
	_, err := r.GetTableDDLInfo(context.Background(), "T")
	require.ErrorContains(t, err, "permission denied")
	_, err = r.GetSequences(context.Background())
	require.ErrorContains(t, err, "permission denied")
}

func TestOracleMySQLAutoColumnRange(t *testing.T) {
	for _, tc := range []struct {
		name                string
		p                   int64
		lo, hi, start, want string
		fail                bool
	}{
		{"integer", 9, "0", "99", "100", "int", false},
		{"empty", 0, "", "", "100", "bigint", false},
		{"large exact", 0, "-1", "9007199254740993", "9007199254740994", "bigint", false},
		{"large precision", 38, "0", "100", "101", "bigint", false},
		{"widen seed", 4, "0", "10", "40000", "bigint", false},
		{"overflow next", 0, "0", "9223372036854775807", "1", "", true},
		{"overflow source", 0, "-9223372036854775809", "1", "1", "", true},
		{"overflow seed", 0, "0", "1", "9223372036854775808", "", true},
		{"fraction", 0, "0", "1.5", "1", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ, err := oracleMySQLAutoColumnType(ColumnInfo{Precision: tc.p}, tc.lo, tc.hi, tc.start)
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, typ)
			}
		})
	}
}

func TestOracleMySQLAutoTablePreflight(t *testing.T) {
	for _, tc := range []struct {
		name                string
		nonnull, fractional int64
		fail                bool
	}{
		{"valid", 3, 0, false}, {"null", 2, 0, true}, {"fractional", 3, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, m := mysqlAutoReader(t)
			r.autoLoaded = true
			r.auto = []OracleMySQLAutoIncrement{{TableName: "T", ColumnName: "Id", TriggerName: "TG", StartValueText: "1000"}}
			m.ExpectQuery("FROM ALL_TAB_COLUMNS").WithArgs("APP", "T").WillReturnRows(sqlmock.NewRows([]string{"name", "type", "len", "precision", "scale", "nullable", "default"}).AddRow("Id", "NUMBER", 0, 0, 0, "Y", nil))
			m.ExpectQuery(`SELECT COUNT\(\*\),COUNT\("Id"\)`).WillReturnRows(sqlmock.NewRows([]string{"count", "nonnull", "fraction", "min", "max"}).AddRow(3, tc.nonnull, tc.fractional, "0", "9007199254740993"))
			info, err := r.GetTableDDLInfo(context.Background(), "T")
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "auto_increment", info.Columns[0].Extra)
				require.Equal(t, "bigint", info.Columns[0].OracleMySQLAutoType)
				require.Equal(t, "NUMBER", info.Columns[0].DataType)
			}
		})
	}
}

func TestOracleMySQLAutoReadPagePreservesExactID(t *testing.T) {
	for _, version := range []int{11, 19} {
		t.Run(string(rune('A'+version)), func(t *testing.T) {
			r, m := mysqlAutoReader(t)
			r.version = version
			r.autoLoaded = true
			r.auto = []OracleMySQLAutoIncrement{{TableName: "T", ColumnName: "Id"}}
			r.pageColumns.Store("T", []string{"Id", "Data"})
			m.ExpectQuery(`TO_CHAR\("Id",'TM9','NLS_NUMERIC_CHARACTERS=''.,'''\) AS "Id"`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Id").OfType("VARCHAR2", ""), sqlmock.NewColumn("Data").OfType("VARCHAR2", "")).AddRow("9007199254740993", "large").AddRow("0", "zero").AddRow("-1", "negative"))
			cols, types, rows, err := r.ReadPage(context.Background(), "T", []string{"Id"}, 0, 10)
			require.NoError(t, err)
			require.Equal(t, []string{"Id", "Data"}, cols)
			require.Equal(t, "NUMBER", types[0])
			require.Equal(t, "9007199254740993", rows[0][0])
			require.Equal(t, "0", rows[1][0])
			require.Equal(t, "-1", rows[2][0])
		})
	}
}

func TestOracleMySQLAutoPartitionStreamUsesExactProjection(t *testing.T) {
	r, m := mysqlAutoReader(t)
	r.autoLoaded = true
	r.auto = []OracleMySQLAutoIncrement{{TableName: "T", ColumnName: "Id"}}
	r.pageColumns.Store("T", []string{"Id"})
	m.ExpectQuery(`SELECT TO_CHAR\("Id",'TM9','NLS_NUMERIC_CHARACTERS=''.,'''\) AS "Id" FROM "APP"\."T"`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("Id").OfType("VARCHAR2", "")).AddRow("9007199254740993").AddRow("0"))
	var values []any
	err := r.StreamPartitionRows(context.Background(), "T", 1, func(cols, types []string, rows [][]any) error {
		require.Equal(t, "NUMBER", types[0])
		values = append(values, rows[0][0])
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []any{"9007199254740993", "0"}, values)
}

func TestOracleMySQLAutoInventoryRejectsConflictsAndMissingSequence(t *testing.T) {
	r, m := mysqlAutoReader(t)
	m.ExpectQuery("FROM ALL_TRIGGERS").WillReturnRows(mysqlAutoTriggerRows().
		AddRow("T", "TG1", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW.ID:=SEQ.NEXTVAL;END;`, nil, "ENABLED", "APP").
		AddRow("T", "TG2", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW.OTHER:=SEQ.NEXTVAL;END;`, nil, "ENABLED", "APP").
		AddRow("U", "TG3", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW.ID:=MISSING.NEXTVAL;END;`, nil, "ENABLED", "OTHER"))
	m.ExpectQuery("FROM ALL_SEQUENCES").WithArgs("APP", "SEQ").WillReturnRows(sqlmock.NewRows([]string{"last", "step", "cycle"}).AddRow("1", "1", "N"))
	m.ExpectQuery("FROM ALL_SEQUENCES").WithArgs("OTHER", "MISSING").WillReturnRows(sqlmock.NewRows([]string{"last", "step", "cycle"}))
	values, err := r.GetOracleMySQLAutoIncrements(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 3)
	require.Contains(t, values[0].Error, "多个")
	require.Contains(t, values[1].Error, "多个")
	require.Contains(t, values[2].Error, "MISSING")
}

func TestOracleMySQLAutoIgnoresNextvalInLiteralsAndComments(t *testing.T) {
	r, m := mysqlAutoReader(t)
	m.ExpectQuery("FROM ALL_TRIGGERS").WillReturnRows(mysqlAutoTriggerRows().AddRow("T", "TG", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW.MSG:='SEQ.NEXTVAL'; /* NEXTVAL */ END;`, nil, "ENABLED", "APP"))
	values, err := r.GetOracleMySQLAutoIncrements(context.Background())
	require.NoError(t, err)
	require.Empty(t, values)
}
