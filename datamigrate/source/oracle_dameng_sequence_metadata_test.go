package source

import (
	"context"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOracleDamengMissingSequenceMetadataKeepsAllSourceReferences(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	r.owner = "AppMixed"
	mock.ExpectQuery("SELECT TABLE_NAME,COLUMN_NAME,DATA_DEFAULT").WithArgs("AppMixed").
		WillReturnRows(sqlmock.NewRows([]string{"table", "column", "default"}))
	mock.ExpectQuery("SELECT TABLE_NAME,TRIGGER_NAME,TRIGGER_TYPE").WithArgs("AppMixed").
		WillReturnRows(sqlmock.NewRows([]string{"table", "trigger", "kind", "event", "body", "when"}).
			AddRow(`Table"One`, "TriggerOne", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW."MixedId" := "AppMixed"."Sq_Mixed_2017".NEXTVAL; END;`, nil).
			AddRow("TableTwo", "TriggerTwo", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW."OtherId" := "AppMixed"."Sq_Mixed_2017".NEXTVAL; END;`, nil).
			AddRow("HealthyTable", "HealthyTrigger", "BEFORE EACH ROW", "INSERT", `BEGIN :NEW.ID := "AppMixed"."HealthySeq".NEXTVAL; END;`, nil))
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("AppMixed", "Sq_Mixed_2017").
		WillReturnRows(sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"}))
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("AppMixed", "HealthySeq").
		WillReturnRows(sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"}).
			AddRow("100", "1", "1", "10000", "0", "N", "N"))

	values, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 2, "a shared missing sequence remains one object")
	missing := values[0]
	require.Equal(t, "metadata", missing.FailureStage)
	require.Equal(t, "Sq_Mixed_2017", missing.Native.Name)
	require.Len(t, missing.Native.Bindings, 2)
	for _, detail := range []string{
		`"AppMixed"."Sq_Mixed_2017"`,
		`"AppMixed"."Table""One"."MixedId"`,
		`"AppMixed"."TableTwo"."OtherId"`,
		`"AppMixed"."TriggerOne"`,
		`"AppMixed"."TriggerTwo"`,
		"源序列是否存在", "访问权限", "同义词", "sql: no rows in result set",
	} {
		require.Contains(t, missing.Unsupported, detail)
	}
	require.Empty(t, missing.StartValueText, "missing metadata must not fabricate a sequence start")
	require.Equal(t, "HealthySeq", values[1].Native.Name)
	require.Equal(t, "100", values[1].StartValueText)
	require.Empty(t, values[1].FailureStage)
	require.Empty(t, values[1].Unsupported)
}

func TestOracleDamengSequenceMetadataObjectFailureHasMetadataStage(t *testing.T) {
	r, mock := oracleDMTestReader(t, 11)
	expectOracleDMFailureSequenceBindings(mock)
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "S_BAD").
		WillReturnError(fmt.Errorf("insufficient privileges"))
	mock.ExpectQuery(`SELECT TO_CHAR\(LAST_NUMBER`).WithArgs("APP", "S_GOOD").
		WillReturnRows(sqlmock.NewRows([]string{"next", "step", "min", "max", "cache", "cycle", "ordered"}).
			AddRow("100", "1", "1", "10000", "0", "N", "N"))

	values, err := r.GetSequences(context.Background())
	require.NoError(t, err)
	require.Len(t, values, 2)
	require.Equal(t, "metadata", values[0].FailureStage)
	require.Contains(t, values[0].Unsupported, `"APP"."S_BAD"`)
	require.Contains(t, values[0].Unsupported, `"APP"."Bad"."ID"`)
	require.Contains(t, values[0].Unsupported, `"APP"."TR_BAD"`)
	require.Contains(t, values[0].Unsupported, "insufficient privileges")
	require.Empty(t, values[1].FailureStage)
	require.Empty(t, values[1].Unsupported)
}
