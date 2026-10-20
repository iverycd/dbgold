package source

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const oracleDMCodeMaxNumberTrigger = `declare
 -- local variables here
 begin
 if inserting and :new.Row_ID is null then
 :new.Row_ID := SEQ_CODE_MAXNUMBER.nextval;
 end if;
end TGR_CODE_MAXNUMBER;`

func TestOracleDamengTriggerConfirmedGrammar(t *testing.T) {
	for _, tc := range []struct {
		name, body, trigger, when, sequence, column string
		onlyWhenNull                                bool
	}{
		{"confirmed source", oracleDMCodeMaxNumberTrigger, "TGR_CODE_MAXNUMBER", "", "SEQ_CODE_MAXNUMBER", "ROW_ID", true},
		{"empty declaration block comment", `DECLARE /* no local declarations */ BEGIN :NEW.Id := Seq.NEXTVAL; END TRG;`, "TRG", "", "SEQ", "ID", false},
		{"quoted end name", `BEGIN IF INSERTING AND :NEW."MixedId" IS NULL THEN SELECT "MixedSeq".NEXTVAL INTO :NEW."MixedId" FROM DUAL; END IF; END "MixedTrigger";`, "MixedTrigger", "", "MixedSeq", "MixedId", true},
		{"existing null guard and when", `BEGIN IF :NEW.ID IS NULL THEN :NEW.ID := SEQ.NEXTVAL; END IF; END;`, "TRG", "NEW.ID IS NULL", "SEQ", "ID", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding, owner, sequence, err := parseOracleDMTrigger(tc.body, tc.when, "AppMixed", tc.trigger)
			require.NoError(t, err)
			require.Equal(t, "AppMixed", owner)
			require.Equal(t, tc.sequence, sequence)
			require.Equal(t, tc.column, binding.ColumnName)
			require.Equal(t, "trigger", binding.Kind)
			require.Equal(t, tc.onlyWhenNull, binding.OnlyWhenNull)
		})
	}
}

func TestOracleDamengTriggerRejectsAdditionalSemantics(t *testing.T) {
	for _, tc := range []struct{ name, body, trigger, when, errorText string }{
		{"variable declaration", `DECLARE v NUMBER; BEGIN :NEW.ID := SEQ.NEXTVAL; END;`, "TRG", "", "仅支持空 DECLARE"},
		{"constant declaration", `DECLARE v CONSTANT NUMBER := 1; BEGIN :NEW.ID := SEQ.NEXTVAL; END;`, "TRG", "", "仅支持空 DECLARE"},
		{"cursor declaration", `DECLARE CURSOR c IS SELECT 1 FROM DUAL; BEGIN :NEW.ID := SEQ.NEXTVAL; END;`, "TRG", "", "仅支持空 DECLARE"},
		{"pragma declaration", `DECLARE PRAGMA AUTONOMOUS_TRANSACTION; BEGIN :NEW.ID := SEQ.NEXTVAL; END;`, "TRG", "", "仅支持空 DECLARE"},
		{"mismatched end name", `BEGIN :NEW.ID := SEQ.NEXTVAL; END ANOTHER_TRIGGER;`, "TRG", "", "END 名称"},
		{"quoted end preserves case", `BEGIN :NEW.ID := SEQ.NEXTVAL; END "mixedTrigger";`, "MixedTrigger", "", "END 名称"},
		{"or condition", `BEGIN IF INSERTING OR :NEW.ID IS NULL THEN :NEW.ID := SEQ.NEXTVAL; END IF; END;`, "TRG", "", "IF 条件"},
		{"additional and condition", `BEGIN IF INSERTING AND :NEW.ID IS NULL AND :NEW.FLAG = 1 THEN :NEW.ID := SEQ.NEXTVAL; END IF; END;`, "TRG", "", "IF 条件"},
		{"different null column", `BEGIN IF INSERTING AND :NEW.OTHER IS NULL THEN :NEW.ID := SEQ.NEXTVAL; END IF; END;`, "TRG", "", "IF 条件"},
		{"non-null condition", `BEGIN IF INSERTING AND :NEW.ID IS NOT NULL THEN :NEW.ID := SEQ.NEXTVAL; END IF; END;`, "TRG", "", "IF 条件"},
		{"updating predicate", `BEGIN IF UPDATING AND :NEW.ID IS NULL THEN :NEW.ID := SEQ.NEXTVAL; END IF; END;`, "TRG", "", "IF 条件"},
		{"when inserting predicate", `BEGIN :NEW.ID := SEQ.NEXTVAL; END;`, "TRG", "INSERTING AND NEW.ID IS NULL", "WHEN 条件"},
		{"exception handler", `BEGIN :NEW.ID := SEQ.NEXTVAL; EXCEPTION WHEN OTHERS THEN NULL; END;`, "TRG", "", "额外语句"},
		{"additional statement", `BEGIN :NEW.ID := SEQ.NEXTVAL; DELETE FROM AUDIT; END;`, "TRG", "", "额外语句"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := parseOracleDMTrigger(tc.body, tc.when, "APP", tc.trigger)
			require.ErrorContains(t, err, tc.errorText)
		})
	}
}

func TestOracleDamengTriggerReaderRetainsRawBodyAndSourceMetadata(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "available sequence"
		if missing {
			name = "missing sequence"
		}
		t.Run(name, func(t *testing.T) {
			r, mock := oracleDMTestReader(t, 11)
			mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("APP").
				WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default"}))
			mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("APP").
				WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}).
					AddRow("CODE_MAXNUMBER", "TGR_CODE_MAXNUMBER", "BEFORE EACH ROW", "INSERT", oracleDMCodeMaxNumberTrigger, nil))
			rows := sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"})
			if !missing {
				rows.AddRow("123", "1", "1", "9999999999999999999999999999", "20", "N", "N")
			}
			mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "SEQ_CODE_MAXNUMBER").WillReturnRows(rows)

			values, err := r.GetSequences(context.Background())
			require.NoError(t, err)
			require.Len(t, values, 1)
			seq := values[0]
			require.Equal(t, "SEQ_CODE_MAXNUMBER", seq.Native.Name)
			require.Equal(t, "ROW_ID", seq.ColumnName)
			require.Len(t, seq.Native.Bindings, 1)
			require.Equal(t, "TGR_CODE_MAXNUMBER", seq.Native.Bindings[0].TriggerName)
			require.Equal(t, oracleDMCodeMaxNumberTrigger, seq.Native.Bindings[0].TriggerBody, "diagnostics retain comments and original spelling")
			require.True(t, seq.Native.Bindings[0].OnlyWhenNull)
			if missing {
				require.Equal(t, "metadata", seq.FailureStage)
				require.Contains(t, seq.Unsupported, `"APP"."SEQ_CODE_MAXNUMBER"`)
				require.Contains(t, seq.Unsupported, `"APP"."TGR_CODE_MAXNUMBER"`)
			} else {
				require.Empty(t, seq.Unsupported)
				require.Equal(t, "9999999999999999999999999999", seq.Native.MaxValue, "Reader must not adapt source bounds")
			}
		})
	}
}

func TestOracleDamengTriggerReaderRejectsWrongHeaderBeforeSequenceLookup(t *testing.T) {
	for _, tc := range []struct{ kind, event string }{
		{"BEFORE EACH ROW", "INSERT OR UPDATE"},
		{"AFTER EACH ROW", "INSERT"},
		{"BEFORE STATEMENT", "INSERT"},
	} {
		t.Run(tc.kind+"/"+tc.event, func(t *testing.T) {
			r, mock := oracleDMTestReader(t, 11)
			mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("APP").
				WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default"}))
			mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("APP").
				WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}).
					AddRow("CODE_MAXNUMBER", "TGR_CODE_MAXNUMBER", tc.kind, tc.event, oracleDMCodeMaxNumberTrigger, "NEW.ROW_ID IS NULL"))
			values, err := r.GetSequences(context.Background())
			require.NoError(t, err)
			require.Len(t, values, 1)
			for _, detail := range []string{`"APP"."TGR_CODE_MAXNUMBER"`, `"APP"."CODE_MAXNUMBER"`, tc.kind, tc.event, "NEW.ROW_ID IS NULL", "仅支持 BEFORE EACH ROW"} {
				require.Contains(t, values[0].Unsupported, detail)
			}
			require.Equal(t, oracleDMCodeMaxNumberTrigger, values[0].Native.Bindings[0].TriggerBody)
		})
	}
}

func TestOracleDamengUnsupportedTriggerIncludesSpecificParserReasonAndRawBody(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	body := strings.Replace(oracleDMCodeMaxNumberTrigger, "-- local variables here", "v NUMBER; -- actual local variable", 1)
	mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("APP").
		WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default"}))
	mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("APP").
		WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}).
			AddRow("CODE_MAXNUMBER", "TGR_CODE_MAXNUMBER", "BEFORE EACH ROW", "INSERT", body, nil))
	values, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Contains(t, values[0].Unsupported, "仅支持空 DECLARE")
	require.Contains(t, values[0].Unsupported, `"APP"."TGR_CODE_MAXNUMBER"`)
	require.Equal(t, body, values[0].Native.Bindings[0].TriggerBody)
}
