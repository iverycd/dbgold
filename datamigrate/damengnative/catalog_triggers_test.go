package damengnative

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNativeTriggerScopeUsesTargetInsteadOfOwner(t *testing.T) {
	for _, tc := range []struct {
		name, ddl, kind, scope string
	}{
		{"database", `CREATE TRIGGER "Other"."Hook" AFTER LOGIN ON DATABASE BEGIN NULL; END;`, "database", ""},
		{"default schema", `CREATE TRIGGER "Other"."Hook" AFTER DDL ON SCHEMA BEGIN NULL; END;`, "schema", "Other"},
		{"cross schema", `CREATE TRIGGER "Other"."Hook" AFTER CREATE ON "App".SCHEMA BEGIN NULL; END;`, "schema", "App"},
		{"unquoted schema", `CREATE TRIGGER "Other"."Hook" BEFORE DROP ON app.SCHEMA BEGIN NULL; END;`, "schema", "APP"},
		{"quoted keyword table", `CREATE TRIGGER "Other"."Hook" AFTER UPDATE ON "SCHEMA" BEGIN NULL; END;`, "table", ""},
		{"qualified keyword table", `CREATE TRIGGER "Other"."Hook" AFTER UPDATE ON "App"."SCHEMA" BEGIN NULL; END;`, "table", ""},
		{"qualified database table", `CREATE TRIGGER "Other"."Hook" AFTER UPDATE ON "App".DATABASE BEGIN NULL; END;`, "table", ""},
		{"body text ignored", `CREATE TRIGGER "Other"."Hook" AFTER UPDATE OF "ON" ON "App"."T" BEGIN PRINT 'AFTER DDL ON DATABASE'; END;`, "table", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, scope, err := nativeTriggerTarget(tc.ddl, "Other")
			require.NoError(t, err)
			require.Equal(t, tc.kind, kind)
			require.Equal(t, tc.scope, scope)
			require.Equal(t, kind != "table", nativeTriggerIsEvent(tc.ddl))
		})
	}
	require.True(t, nativeTriggerIsEvent(`CREATE TRIGGER T AFTER DDL ON 'unterminated`))
	require.True(t, triggerOnDatabase(`CREATE TRIGGER T AFTER DDL`))
}

func TestTargetCrossSchemaEventHookBlocksBeforeMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}}
	mock.ExpectQuery(`SELECT s.NAME,o.NAME,o.INFO1`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "flags", "status"}).
		AddRow("Other", "CrossScope", 4, "Y").
		AddRow("Other", "Unrelated", 4, "Y").
		AddRow("Other", "EngineGenerated", 4|32, "Y"))
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL\('TRIGGER',\?,\?\)`).WithArgs("CrossScope", "Other").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "Other"."CrossScope" AFTER DDL ON "App".SCHEMA BEGIN NULL; END;`))
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL\('TRIGGER',\?,\?\)`).WithArgs("Unrelated", "Other").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "Other"."Unrelated" AFTER DDL ON "Different".SCHEMA BEGIN NULL; END;`))
	require.NoError(t, r.checkActiveEventTriggers())
	require.Len(t, r.snapshot.Issues, 1)
	require.Equal(t, `"Other"."CrossScope"`, r.snapshot.Issues[0].Name)
	require.Equal(t, "error", r.snapshot.Issues[0].Severity)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTargetEventScopeUnknownBlocksBeforeMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{}}
	mock.ExpectQuery(`SELECT s.NAME,o.NAME,o.INFO1`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "flags", "status"}).AddRow("Other", "Unreadable", 4, "Y"))
	mock.ExpectQuery(`SELECT DBMS_METADATA.GET_DDL\('TRIGGER',\?,\?\)`).WithArgs("Unreadable", "Other").WillReturnRows(sqlmock.NewRows([]string{"ddl"}).AddRow(`CREATE TRIGGER "Other"."Unreadable" AFTER DDL`))
	require.ErrorContains(t, r.checkActiveEventTriggers(), "已阻止目标修改")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTargetEventEnabledStateUsesCatalogStatusInsteadOfGuessedBit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &catalogReader{ctx: context.Background(), db: db, schema: "App", snapshot: &Snapshot{Objects: []Object{{Kind: "trigger", Name: "Disabled", EventTrigger: true}}}, index: map[string]int{"trigger\x00Disabled": 0}}
	mock.ExpectQuery(`SELECT s.NAME,o.NAME,o.INFO1`).WillReturnRows(sqlmock.NewRows([]string{"owner", "name", "flags", "status"}).AddRow("App", "Disabled", 4100, "N"))
	require.NoError(t, r.checkActiveEventTriggers())
	require.Empty(t, r.snapshot.Issues)
	require.Equal(t, "DISABLED", r.snapshot.Objects[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}
