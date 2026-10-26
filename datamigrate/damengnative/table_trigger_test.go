package damengnative

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManualTableTriggerCompleteProgram(t *testing.T) {
	program := `-- original program
CREATE OR REPLACE TRIGGER "App"."Audit" BEFORE INSERT OR UPDATE OF "Value", ID OR DELETE LOCAL ON "App"."Orders"
REFERENCING OLD AS OLD NEW AS NEW FOR EACH ROW WHEN (NEW.ID > 0)
DECLARE
  message VARCHAR(100) := q'[END; DROP TABLE Outside;]';
  PROCEDURE write_log IS
  BEGIN INSERT INTO "App"."Log" VALUES ('MiX'); END;
BEGIN
  IF :NEW.ID > 0 THEN
    FOR i IN 1..3 LOOP
      UPDATE "App"."Log" SET VALUE = CASE WHEN i=1 THEN 'END IF;' ELSE 'x' END;
    END LOOP;
  ELSE
    DELETE FROM "App"."Log" WHERE VALUE = '/*! literal */';
  END IF;
  BEGIN write_log; END;
END "Audit";
/
`
	d, err := ParseTableTriggerSQL(program, "Other")
	require.NoError(t, err)
	require.Equal(t, "App", d.Schema)
	require.Equal(t, "Audit", d.Name)
	require.Equal(t, "App", d.TableSchema)
	require.Equal(t, "Orders", d.Table)
	require.True(t, d.Replace)
	require.NotContains(t, d.SQL, "\n/")
	require.Contains(t, d.CreateSQL(false), `CREATE TRIGGER "App"."Audit"`)
	require.Contains(t, d.CreateSQL(false), `'/*! literal */'`)
}

func TestManualTableTriggerRejectsExtraStatementsAndWrongScope(t *testing.T) {
	for _, program := range []string{
		`CREATE TRIGGER A AFTER LOGIN ON DATABASE BEGIN NULL; END;`,
		`CREATE TRIGGER A AFTER DDL ON SCHEMA BEGIN NULL; END;`,
		`CREATE TRIGGER A INSTEAD OF INSERT ON V BEGIN NULL; END;`,
		`CREATE TRIGGER A AFTER INSERT ON T; BEGIN NULL; END;`,
		`CREATE TRIGGER A AFTER INSERT ON T BEGIN NULL; END; DROP TABLE OTHER;`,
		`CREATE TRIGGER A AFTER INSERT ON T BEGIN NULL; END; BEGIN NULL; END;`,
		`CREATE TRIGGER A AFTER INSERT ON T BEGIN IF 1=1 THEN NULL; END;`,
		`CREATE TRIGGER A AFTER INSERT ON T BEGIN x := 'unterminated; END;`,
		`CREATE TRIGGER A AFTER INSERT ON T BEGIN NULL; END; / SELECT 1;`,
		`CREATE TRIGGER A AFTER INSERT ON T BEGIN NULL; /*! DROP TABLE OTHER */ END;`,
		`CREATE TRIGGER A AFTER INSERT ON T@REMOTE BEGIN NULL; END;`,
	} {
		_, err := ParseTableTriggerSQL(program, "APP")
		require.Error(t, err, program)
	}
}

func TestManualTableTriggerSingleStatementAndForwardDeclaration(t *testing.T) {
	for _, program := range []string{
		`CREATE TRIGGER A AFTER INSERT ON T FOR EACH ROW INSERT INTO LOG VALUES('semi; colon');`,
		`CREATE TRIGGER A AFTER INSERT ON T DECLARE PROCEDURE p; x INT; BEGIN NULL; END;`,
		`CREATE TRIGGER A AFTER INSERT ON T DECLARE FUNCTION f RETURN INT IS x INT; BEGIN RETURN 1; END; BEGIN NULL; END;`,
	} {
		_, err := ParseTableTriggerSQL(program, "APP")
		require.NoError(t, err, program)
	}
}

func TestManualTableTriggerHeaderRecoversFailedBody(t *testing.T) {
	for _, body := range []string{`BEGIN broken`, `BEGIN x := 'unterminated`, `DECLARE x VARCHAR(10) := q'[unterminated`, `BEGIN /* unclosed`} {
		d, err := ParseTableTriggerHeader(`CREATE OR REPLACE TRIGGER "App"."Audit" BEFORE UPDATE OF "ON" LOCAL ON "App"."Orders" `+body, "Other")
		require.NoError(t, err, body)
		require.Equal(t, "App", d.Schema)
		require.Equal(t, "Orders", d.Table)
	}
	_, err := ParseTableTriggerHeader(`CREATE TRIGGER A INSTEAD OF INSERT ON V BEGIN NULL; END;`, "APP")
	require.Error(t, err)
}

func TestManualTableTriggerDefinitionEquivalence(t *testing.T) {
	a := `CREATE OR REPLACE TRIGGER APP.A AFTER INSERT ON APP.T FOR EACH ROW BEGIN INSERT INTO APP.LOG VALUES('MiX',q'[a; END;]'); END;`
	b := `create trigger "APP"."A" after insert on "APP"."T" for each row
begin /* retained meaning */ insert into "APP"."LOG" values('MiX',q'[a; END;]'); end;
/
`
	require.True(t, TableTriggerDefinitionsEqual(a, b, "APP"))
	require.True(t, TableTriggerDefinitionsEqual(a, strings.ReplaceAll(a, "APP.A", "A"), "APP"))
	require.False(t, TableTriggerDefinitionsEqual(a, strings.ReplaceAll(b, "'MiX'", "'MIX'"), "APP"))
	require.False(t, TableTriggerDefinitionsEqual(a, strings.ReplaceAll(b, `"LOG"`, `"Log"`), "APP"))
	require.False(t, TableTriggerDefinitionsEqual(a, a+" DROP TABLE OTHER;", "APP"))
}
