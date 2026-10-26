package mysqldmtrigger

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

func triggerFixture(body string) (source.TriggerInfo, map[string]*source.TableDDLInfo, Options) {
	info := source.TriggerInfo{Schema: "Src", TableName: "Orders", Name: "OrderAudit", Timing: "BEFORE", Event: "UPDATE", Body: body, SourceDDL: "CREATE TRIGGER OrderAudit BEFORE UPDATE ON Orders FOR EACH ROW " + body, SQLMode: "STRICT_TRANS_TABLES"}
	tables := map[string]*source.TableDDLInfo{"Orders": {TableName: "Orders", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}, {Name: "Amount", DataType: "decimal", Precision: 12, Scale: 2}, {Name: "Note", DataType: "varchar", Length: 100}, {Name: "Stamp", DataType: "datetime", DateTimePrecision: 3}}}, "Audit": {TableName: "Audit", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}, {Name: "Amount", DataType: "decimal", Precision: 12, Scale: 2}, {Name: "Note", DataType: "varchar", Length: 100}}}}
	opt := Options{Schema: "Dest", TableNames: map[string]string{"Orders": "OrderData", "Audit": "AuditLog"}, ColumnNames: map[string]map[string]string{"Orders": {"Amount": "AmountValue"}, "Audit": {"Amount": "RecordedAmount"}}}
	return info, tables, opt
}

func TestConvertCompleteProgramAndImmutableMappings(t *testing.T) {
	info, tables, opt := triggerFixture(`BEGIN DECLARE v INT DEFAULT 1; SET v=v+1, NEW.Amount=v; IF NEW.Amount>0 THEN INSERT INTO Audit(Id,Amount) VALUES(NEW.Id,NEW.Amount); ELSEIF NEW.Amount IS NULL THEN SET NEW.Amount=0; ELSE SET NEW.Amount=-NEW.Amount; END IF; END`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Equal(t, []string{"AUDITLOG", "ORDERDATA"}, p.Dependencies)
	for _, s := range []string{`CREATE TRIGGER "DEST"."ORDERAUDIT" BEFORE UPDATE ON "DEST"."ORDERDATA" FOR EACH ROW`, `"DBGOLD_VAR_1" INT`, `:NEW."AMOUNTVALUE" :=`, `INSERT INTO "DEST"."AUDITLOG" ("ID", "RECORDEDAMOUNT")`, `ELSIF`} {
		require.Contains(t, p.TriggerSQL, s)
	}
	opt.ColumnNames["Orders"]["Amount"] = "Changed"
	tables["Orders"].Columns[1].Name = "Changed"
	require.Equal(t, "AmountValue", p.Options.ColumnNames["Orders"]["Amount"])
	require.Equal(t, "Amount", p.SourceTables[1].Columns[1].Name)
	require.NoError(t, ValidatePlan(p))
	p.TriggerSQL += " DELETE FROM X;"
	require.Error(t, ValidatePlan(p))
}

func TestConvertControlFlowAndLexicalScopes(t *testing.T) {
	info, tables, opt := triggerFixture(`outer_block: BEGIN DECLARE n INT DEFAULT 0; a: WHILE n<3 DO SET n=n+1; IF n=1 THEN ITERATE a; END IF; b: LOOP LEAVE b; END LOOP b; END WHILE a; r: REPEAT SET n=n+1; IF n<6 THEN ITERATE r; END IF; UNTIL n>7 END REPEAT r; CASE n WHEN 8 THEN SET NEW.Amount=8; ELSE CASE WHEN n=9 THEN SET NEW.Amount=9; ELSE SET NEW.Amount=0; END CASE; END CASE; BEGIN DECLARE n INT DEFAULT 99; SET NEW.Amount=n; END; LEAVE outer_block; SET NEW.Amount=1; END outer_block`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, "CONTINUE DBGOLD_LABEL_")
	require.Contains(t, p.TriggerSQL, "EXIT WHEN")
	require.Contains(t, p.TriggerSQL, "END CASE;")
	// ITERATE precedes and skips the REPEAT's bottom condition; it must not
	// become a precondition loop that evaluates UNTIL before every iteration.
	i := strings.LastIndex(p.TriggerSQL, "CONTINUE DBGOLD_LABEL_")
	until := strings.Index(p.TriggerSQL[i:], "EXIT WHEN")
	require.Greater(t, until, 0)
	require.NotContains(t, p.TriggerSQL, "FIRST_ITERATION")
}

func TestSelectIntoPreservesNoRowsAndDMLReferences(t *testing.T) {
	info, tables, opt := triggerFixture(`BEGIN DECLARE x INT DEFAULT 7; SELECT Id INTO x FROM Audit WHERE Id=NEW.Id; SET NEW.Amount=x; INSERT INTO Audit(Id,Amount) SELECT Id,NEW.Amount FROM Orders WHERE Id=NEW.Id; UPDATE Audit SET Amount=Amount+NEW.Amount WHERE Id=NEW.Id; DELETE FROM Audit WHERE Id=OLD.Id; END`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, "EXCEPTION WHEN NO_DATA_FOUND THEN NULL;")
	selectPos := strings.Index(p.TriggerSQL, `SELECT "DEST"."AUDITLOG"."ID" INTO "DBGOLD_INTO_`)
	require.Greater(t, selectPos, 0)
	assignPos := strings.Index(p.TriggerSQL[selectPos:], `"DBGOLD_VAR_1" := "DBGOLD_INTO_`)
	handlerPos := strings.Index(p.TriggerSQL[selectPos:], "EXCEPTION WHEN NO_DATA_FOUND")
	require.Greater(t, assignPos, 0)
	require.Greater(t, handlerPos, assignPos)
	require.Contains(t, p.TriggerSQL, `UPDATE "DEST"."AUDITLOG" SET "RECORDEDAMOUNT"`)
	require.Contains(t, p.TriggerSQL, `DELETE FROM "DEST"."AUDITLOG"`)
}

func TestSignalAndPredecessorProof(t *testing.T) {
	info, tables, opt := triggerFixture(`BEGIN IF NEW.Amount<0 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='amount; cannot be negative'; END IF; END`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.Contains(t, p.TriggerSQL, "RAISE_APPLICATION_ERROR(-20000, 'amount; cannot be negative')")
	previous := p
	info.Name = "LaterAudit"
	opt.Predecessor = previous.Name
	opt.PredecessorSQLs = map[string]string{previous.Name: previous.TriggerSQL}
	p, err = Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, `FOLLOWS "DEST"."ORDERAUDIT"`)
	p.Options.PredecessorSQLs[previous.Name] = "CREATE TRIGGER wrong " + ToolMarker
	require.Error(t, ValidatePlan(p))
}

func TestConvertRejectsWholeUnsupportedPrograms(t *testing.T) {
	for _, body := range []string{`BEGIN SET NEW.Amount=1; CALL secret(); END`, `BEGIN DECLARE CONTINUE HANDLER FOR NOT FOUND SET NEW.Amount=0; END`, `BEGIN DECLARE cur CURSOR FOR SELECT Id FROM Audit; END`, `BEGIN SET @session=1; END`, `BEGIN x: BEGIN ITERATE x; END x; END`, `BEGIN LEAVE missing; END`, `BEGIN DECLARE n INT; SELECT Id INTO n FROM other.Audit; END`, `INSERT IGNORE INTO Audit(Id) VALUES(1)`, `UPDATE Audit SET Id=1,Amount=Id`, `SIGNAL SQLSTATE '02000'`, `SET NEW.Amount=(SELECT Id FROM Audit)`, `SET NEW.Amount=unknown_function(1)`, `BEGIN SET NEW.Amount=1; COMMIT; END`, `BEGIN SET NEW.Amount=1; PREPARE p FROM 'DELETE FROM X'; END`} {
		t.Run(body, func(t *testing.T) {
			info, tables, opt := triggerFixture(body)
			p, err := Convert(info, tables, opt)
			require.Error(t, err)
			require.Empty(t, p.TriggerSQL)
			require.Contains(t, err.Error(), "第")
		})
	}
}

func TestSQLModeNamesMetadataAndEvents(t *testing.T) {
	info, tables, opt := triggerFixture(`SET NEW.Note='O\'Brien; literal'`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.Contains(t, p.TriggerSQL, "'O''Brien; literal'")
	info.SQLMode = "NO_BACKSLASH_ESCAPES"
	_, err = Convert(info, tables, opt)
	require.Error(t, err)
	info, tables, opt = triggerFixture(`SET NEW.Amount=1`)
	info.SQLMode = "PIPES_AS_CONCAT"
	_, err = Convert(info, tables, opt)
	require.ErrorContains(t, err, "SQL_MODE")
	info.SQLMode = ""
	info.MetadataError = "TRIGGER denied"
	_, err = Convert(info, tables, opt)
	require.ErrorContains(t, err, "TRIGGER denied")
	info.MetadataError = ""
	info.SourceDDL = ""
	_, err = Convert(info, tables, opt)
	require.ErrorContains(t, err, "CREATE TRIGGER")
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		for _, timing := range []string{"BEFORE", "AFTER"} {
			info, tables, opt := triggerFixture(`INSERT INTO Audit(Id) VALUES(1)`)
			info.Event, info.Timing = event, timing
			p, err := Convert(info, tables, opt)
			require.NoError(t, err)
			require.Contains(t, p.TriggerSQL, timing+" "+event)
		}
	}
}

func TestAggregateSelectIntoAndLocalPrecedence(t *testing.T) {
	info, tables, opt := triggerFixture(`BEGIN DECLARE Id INT DEFAULT 7; DECLARE n INT; DECLARE total DECIMAL(12,2); SELECT COUNT(*),SUM(Amount) INTO n,total FROM Audit WHERE Audit.Id=NEW.Id; SELECT Id INTO n FROM Audit WHERE Audit.Id=NEW.Id; SET NEW.Amount=total+n; END`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, `COUNT(1)`)
	require.Contains(t, p.TriggerSQL, `SUM("DEST"."AUDITLOG"."RECORDEDAMOUNT")`)
	require.Contains(t, p.TriggerSQL, `SELECT DBGOLD_VAR_1 INTO`)
	for _, body := range []string{
		`BEGIN DECLARE n INT; SELECT COUNT(*)+Id INTO n FROM Audit; END`,
		`BEGIN DECLARE n INT; SELECT COUNT(DISTINCT Id) INTO n FROM Audit; END`,
		`BEGIN DECLARE n INT; SELECT SUM(MAX(Id)) INTO n FROM Audit; END`,
		`SET NEW.Amount=SUM(NEW.Amount)`,
		`BEGIN DECLARE n INT; SELECT Id INTO n FROM Audit WHERE COUNT(*)>0; END`,
	} {
		info, tables, opt := triggerFixture(body)
		p, err := Convert(info, tables, opt)
		require.Error(t, err, body)
		require.Empty(t, p.TriggerSQL)
	}
}

func TestEmptyBlocksAndTruncatedAssignments(t *testing.T) {
	for _, body := range []string{`BEGIN END`, `BEGIN IF NEW.Id>0 THEN END IF; END`, `BEGIN CASE NEW.Id WHEN 1 THEN END CASE; END`, `BEGIN x: LOOP LEAVE x; END LOOP x; END`} {
		info, tables, opt := triggerFixture(body)
		p, err := Convert(info, tables, opt)
		require.NoError(t, err, body)
		require.NoError(t, ValidatePlan(p))
		if body != `BEGIN x: LOOP LEAVE x; END LOOP x; END` {
			require.Contains(t, p.TriggerSQL, "NULL;", body)
		}
	}
	for _, body := range []string{`SET NEW.Amount=`, `SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT=`} {
		info, tables, opt := triggerFixture(body)
		require.NotPanics(t, func() { p, err := Convert(info, tables, opt); require.Error(t, err); require.Empty(t, p.TriggerSQL) })
	}
}

func TestMySQLBooleanLiteralsAndNewTimestampPrecision(t *testing.T) {
	info, tables, opt := triggerFixture(`BEGIN SET NEW.Amount=TRUE+1; SET NEW.Stamp=NOW(6); IF FALSE THEN SET NEW.Amount=0; END IF; END`)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.Contains(t, p.TriggerSQL, `:= (1 + 1)`)
	require.Contains(t, p.TriggerSQL, `AS TIMESTAMP(3))`)
	require.Contains(t, p.TriggerSQL, `IF (0 <> 0)`)
	require.NoError(t, ValidatePlan(p))
}

func TestStrictDivisionByZeroDoesNotBecomeSuccessfulNullWrite(t *testing.T) {
	for _, mode := range []string{"STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO", "STRICT_ALL_TABLES,ERROR_FOR_DIVISION_BY_ZERO"} {
		for _, expr := range []string{`NEW.Amount/0`, `NEW.Amount DIV 0`, `NEW.Amount%0`, `MOD(NEW.Amount,0)`, `NEW.Amount/NEW.Id`, `NEW.Amount DIV NEW.Id`, `NEW.Amount%NEW.Id`, `MOD(NEW.Amount,NEW.Id)`, `NEW.Amount/(1-1)`, `NEW.Amount/0.000`} {
			info, tables, opt := triggerFixture("SET NEW.Amount=" + expr)
			info.SQLMode = mode
			p, err := Convert(info, tables, opt)
			require.ErrorContains(t, err, "除零异常", mode+": "+expr)
			require.Empty(t, p.TriggerSQL)
		}
		for _, expr := range []string{`NEW.Amount/2`, `NEW.Amount DIV -2`, `NEW.Amount%(+2)`, `MOD(NEW.Amount,(-2))`, `NEW.Amount/0.001`, `NEW.Amount/(-0.001)`} {
			info, tables, opt := triggerFixture("SET NEW.Amount=" + expr)
			info.SQLMode = mode
			p, err := Convert(info, tables, opt)
			require.NoError(t, err, mode+": "+expr)
			require.NoError(t, ValidatePlan(p))
		}
	}
	for _, mode := range []string{"", "STRICT_TRANS_TABLES", "ERROR_FOR_DIVISION_BY_ZERO"} {
		info, tables, opt := triggerFixture(`SET NEW.Amount=NEW.Amount/NEW.Id`)
		info.SQLMode = mode
		p, err := Convert(info, tables, opt)
		require.NoError(t, err, mode)
		require.Contains(t, p.TriggerSQL, "NULLIF(")
	}
}
