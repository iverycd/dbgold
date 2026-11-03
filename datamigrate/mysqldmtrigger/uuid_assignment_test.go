package mysqldmtrigger

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

const compactUUIDSourceExpression = "REPLACE(UUID(), '-', '')"

func uuidAssignmentFixture(body string) (source.TriggerInfo, map[string]*source.TableDDLInfo, Options) {
	info := source.TriggerInfo{
		Schema: "Src", TableName: "jwjd_ajspxx", Name: "tri_jwjd_ajspxx_rowguid",
		Timing: "BEFORE", Event: "INSERT", Body: body, SQLMode: "STRICT_TRANS_TABLES",
		SourceDDL: "CREATE DEFINER=`root`@`%` TRIGGER `tri_jwjd_ajspxx_rowguid` BEFORE INSERT ON `jwjd_ajspxx` FOR EACH ROW " + body,
	}
	tables := map[string]*source.TableDDLInfo{
		info.TableName: {TableName: info.TableName, Columns: []source.ColumnInfo{
			{Name: "RowGuid", DataType: "varchar", Length: 32},
			{Name: "Amount", DataType: "int"},
		}},
	}
	opt := Options{
		Schema:      "Dest",
		TableNames:  map[string]string{info.TableName: "CaseData"},
		ColumnNames: map[string]map[string]string{info.TableName: {"RowGuid": "RowToken"}},
	}
	return info, tables, opt
}

func TestUUIDAssignmentConvertsWholeRHSWithScalarWrappers(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"direct", "SET NEW.RowGuid = " + compactUUIDSourceExpression},
		{"screenshot", "BEGIN\n\nSET NEW.RowGuid = (\n    SELECT\n        REPLACE (uuid(), '-', '')\n);\n\nEND"},
		{"case_and_space", "SET NEW.RowGuid = rEpLaCe ( uUiD ( ), '-', '' )"},
		{"parentheses", "SET NEW.RowGuid = (((REPLACE((UUID()), ('-'), ('')))))"},
		{"scalar_select", "SET NEW.RowGuid = (SELECT " + compactUUIDSourceExpression + ")"},
		{"nested_scalar_select", "SET NEW.RowGuid = ((SELECT ((SELECT (" + compactUUIDSourceExpression + ")))))"},
		{"ordinary_comment", "SET NEW.RowGuid = (SELECT /* FROM SQL_CACHE */ " + compactUUIDSourceExpression + ")"},
	} {
		for _, emptyStringIsNull := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty_is_null=%t", tc.name, emptyStringIsNull), func(t *testing.T) {
				info, tables, opt := uuidAssignmentFixture(tc.body)
				opt.Capabilities.EmptyStringIsNull = emptyStringIsNull
				p, err := Convert(info, tables, opt)
				require.NoError(t, err)
				require.NoError(t, ValidatePlan(p))
				require.Equal(t, info, p.Source, "source case and complete original DDL must remain unchanged")
				require.Nil(t, p.Options.CharacterMetadata, "UUID generation must not require source character metadata")
				require.Equal(t, []string{"CASEDATA"}, p.Dependencies)
				require.Contains(t, p.TriggerSQL, `BEFORE INSERT ON "DEST"."CASEDATA" FOR EACH ROW`)
				require.Contains(t, p.TriggerSQL, `:NEW."ROWTOKEN" := LOWER(RAWTOHEX(SYS_GUID()));`)
				require.Equal(t, 1, strings.Count(p.TriggerSQL, "SYS_GUID()"), "one source UUID call must remain one target call")
				require.NotContains(t, p.TriggerSQL, "SELECT")
				require.NotContains(t, p.TriggerSQL, "DEFINER")
				require.NotContains(t, strings.ToUpper(p.TriggerSQL), "UUID()")
				p.TriggerSQL = strings.Replace(p.TriggerSQL, "LOWER(RAWTOHEX(SYS_GUID()))", "'forged'", 1)
				require.Error(t, ValidatePlan(p))
			})
		}
	}
}

func TestUUIDAssignmentSupportsLocalAndMultipleSetTargets(t *testing.T) {
	info, tables, opt := uuidAssignmentFixture("BEGIN DECLARE v VARCHAR(32); SET v=" + compactUUIDSourceExpression + "; SET NEW.RowGuid=v; END")
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, `"DBGOLD_VAR_1" := LOWER(RAWTOHEX(SYS_GUID()));`)
	require.Equal(t, 1, strings.Count(p.TriggerSQL, "SYS_GUID()"))

	info, tables, opt = uuidAssignmentFixture("SET NEW.RowGuid=" + compactUUIDSourceExpression + ", NEW.RowGuid=" + compactUUIDSourceExpression)
	p, err = Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Equal(t, 2, strings.Count(p.TriggerSQL, "SYS_GUID()"), "separate assignments each generate their own value")
}

func TestBareUUIDAssignmentConvertsWholeRHSWithScalarWrappers(t *testing.T) {
	for _, tc := range []struct{ name, rhs string }{
		{"direct", "UUID()"},
		{"case_and_space", "uUiD ( )"},
		{"parentheses", "(((UUID())))"},
		{"scalar_select", "(SELECT UUID())"},
		{"nested_scalar_select", "((SELECT ((SELECT (UUID())))))"},
		{"ordinary_comment", "(SELECT /* FROM SQL_CACHE */ UUID())"},
	} {
		for _, emptyStringIsNull := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty_is_null=%t", tc.name, emptyStringIsNull), func(t *testing.T) {
				info, tables, opt := uuidAssignmentFixture("SET NEW.RowGuid=" + tc.rhs)
				tables[info.TableName].Columns[0].Length = 36
				opt.Capabilities.EmptyStringIsNull = emptyStringIsNull
				p, err := Convert(info, tables, opt)
				require.NoError(t, err)
				require.NoError(t, ValidatePlan(p))
				require.Equal(t, info, p.Source)
				require.Nil(t, p.Options.CharacterMetadata)
				require.Equal(t, []string{"CASEDATA"}, p.Dependencies)
				require.Contains(t, p.TriggerSQL, "DECLARE\n\"DBGOLD_UUID_1\" VARCHAR(32);\nBEGIN\n\"DBGOLD_UUID_1\" := LOWER(RAWTOHEX(SYS_GUID()));\n")
				require.Contains(t, p.TriggerSQL, `:NEW."ROWTOKEN" := SUBSTR("DBGOLD_UUID_1",1,8) || '-' || SUBSTR("DBGOLD_UUID_1",9,4) || '-' || SUBSTR("DBGOLD_UUID_1",13,4) || '-' || SUBSTR("DBGOLD_UUID_1",17,4) || '-' || SUBSTR("DBGOLD_UUID_1",21,12);`)
				require.Equal(t, 1, strings.Count(p.TriggerSQL, "SYS_GUID()"))
				require.NotContains(t, p.TriggerSQL, "SELECT")
				require.NotContains(t, p.TriggerSQL, "DEFINER")
				require.NotContains(t, strings.ToUpper(p.TriggerSQL), "UUID()")
				p.TriggerSQL = strings.Replace(p.TriggerSQL, `SUBSTR("DBGOLD_UUID_1",21,12)`, `SUBSTR("DBGOLD_UUID_1",21,11)`, 1)
				require.Error(t, ValidatePlan(p))
			})
		}
	}
}

func TestBareUUIDAssignmentConvertsScreenshotDefinition(t *testing.T) {
	body := "BEGIN\n\nSET NEW.ROWGUID=UUID();\n\nEND"
	info := source.TriggerInfo{
		Schema: "Src", TableName: "jwjd_jyzd_jq", Name: "JWJD_JYZD_JQ_trigger",
		Timing: "BEFORE", Event: "INSERT", Body: body, SQLMode: "STRICT_TRANS_TABLES",
		SourceDDL: "CREATE DEFINER=`root`@`%` TRIGGER `JWJD_JYZD_JQ_trigger` BEFORE INSERT ON `jwjd_jyzd_jq` FOR EACH ROW " + body,
	}
	tables := map[string]*source.TableDDLInfo{
		info.TableName: {TableName: info.TableName, Columns: []source.ColumnInfo{{Name: "ROWGUID", DataType: "varchar", Length: 36}}},
	}
	p, err := Convert(info, tables, Options{Schema: "Dest"})
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Equal(t, info, p.Source)
	require.Contains(t, p.TriggerSQL, `CREATE TRIGGER "DEST"."JWJD_JYZD_JQ_TRIGGER" BEFORE INSERT ON "DEST"."JWJD_JYZD_JQ" FOR EACH ROW`)
	require.Contains(t, p.TriggerSQL, `:NEW."ROWGUID" := SUBSTR(`)
	require.Equal(t, 1, strings.Count(p.TriggerSQL, "SYS_GUID()"))
}

func TestBareUUIDAssignmentSupportsLocalAndOrderedMultipleSetTargets(t *testing.T) {
	info, tables, opt := uuidAssignmentFixture("BEGIN DECLARE v VARCHAR(36); SET v=UUID(), NEW.RowGuid=v, v=UUID(), NEW.RowGuid=v; END")
	tables[info.TableName].Columns[0].Length = 36
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Equal(t, 2, strings.Count(p.TriggerSQL, "SYS_GUID()"))
	variables := regexp.MustCompile(`"DBGOLD_UUID_\d+" VARCHAR\(32\);`).FindAllString(p.TriggerSQL, -1)
	require.Len(t, variables, 2)
	require.NotEqual(t, variables[0], variables[1], "each assignment must scope its own generated UUID")
	firstValue := strings.Index(p.TriggerSQL, "SYS_GUID()")
	firstRead := strings.Index(p.TriggerSQL, `:NEW."ROWTOKEN" := DBGOLD_VAR_1;`)
	secondValue := strings.LastIndex(p.TriggerSQL, "SYS_GUID()")
	secondRead := strings.LastIndex(p.TriggerSQL, `:NEW."ROWTOKEN" := DBGOLD_VAR_1;`)
	require.Positive(t, firstRead)
	require.Less(t, firstValue, firstRead)
	require.Less(t, firstRead, secondValue)
	require.Less(t, secondValue, secondRead)
	require.Equal(t, 2, strings.Count(p.TriggerSQL, `"DBGOLD_VAR_1" := SUBSTR(`))

	info, tables, opt = uuidAssignmentFixture("SET NEW.RowGuid=UUID(), NEW.RowGuid=UUID()")
	tables[info.TableName].Columns[0].Length = 36
	p, err = Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Equal(t, 2, strings.Count(p.TriggerSQL, "SYS_GUID()"))
	require.Equal(t, 2, strings.Count(p.TriggerSQL, `:NEW."ROWTOKEN" := SUBSTR(`))
}

func TestUUIDAssignmentRejectsQueriesAndPartialPatterns(t *testing.T) {
	for _, tc := range []struct{ name, rhs string }{
		{"table", "(SELECT " + compactUUIDSourceExpression + " FROM jwjd_ajspxx)"},
		{"dual", "(SELECT " + compactUUIDSourceExpression + " FROM DUAL)"},
		{"where", "(SELECT " + compactUUIDSourceExpression + " WHERE TRUE)"},
		{"aggregate", "(SELECT MAX(" + compactUUIDSourceExpression + "))"},
		{"group_by", "(SELECT " + compactUUIDSourceExpression + " GROUP BY 1)"},
		{"having", "(SELECT " + compactUUIDSourceExpression + " HAVING TRUE)"},
		{"order_by", "(SELECT " + compactUUIDSourceExpression + " ORDER BY 1)"},
		{"limit", "(SELECT " + compactUUIDSourceExpression + " LIMIT 1)"},
		{"union", "(SELECT " + compactUUIDSourceExpression + " UNION SELECT " + compactUUIDSourceExpression + ")"},
		{"alias", "(SELECT " + compactUUIDSourceExpression + " AS id)"},
		{"multiple_fields", "(SELECT " + compactUUIDSourceExpression + ", 1)"},
		{"distinct", "(SELECT DISTINCT " + compactUUIDSourceExpression + ")"},
		{"explicit_all", "(SELECT ALL " + compactUUIDSourceExpression + ")"},
		{"no_cache", "(SELECT SQL_NO_CACHE " + compactUUIDSourceExpression + ")"},
		{"cache", "(SELECT SQL_CACHE " + compactUUIDSourceExpression + ")"},
		{"small_result", "(SELECT SQL_SMALL_RESULT " + compactUUIDSourceExpression + ")"},
		{"optimizer_hint", "(SELECT /*+ MAX_EXECUTION_TIME(1) */ " + compactUUIDSourceExpression + ")"},
		{"uuid_argument", "REPLACE(UUID(1), '-', '')"},
		{"extra_replace_argument", "REPLACE(UUID(), '-', '', 'x')"},
		{"missing_replace_argument", "REPLACE(UUID(), '-')"},
		{"schema_uuid", "REPLACE(Src.UUID(), '-', '')"},
		{"schema_replace", "Src.REPLACE(UUID(), '-', '')"},
		{"other_search", "REPLACE(UUID(), '_', '')"},
		{"other_replacement", "REPLACE(UUID(), '-', 'x')"},
		{"computed_search", "REPLACE(UUID(), CHAR(45), '')"},
		{"null_search", "REPLACE(UUID(), NULL, '')"},
		{"concat", "CONCAT(" + compactUUIDSourceExpression + ", 'x')"},
		{"ifnull", "IFNULL(" + compactUUIDSourceExpression + ", 'x')"},
		{"case", "CASE WHEN TRUE THEN " + compactUUIDSourceExpression + " ELSE 'x' END"},
		{"comparison", compactUUIDSourceExpression + " = 'x'"},
		{"arithmetic", compactUUIDSourceExpression + " + 1"},
		{"nested_query_in_uuid_argument", "REPLACE(UUID((SELECT 1)), '-', '')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, tables, opt := uuidAssignmentFixture("BEGIN SET NEW.Amount=1; SET NEW.RowGuid=" + tc.rhs + "; END")
			p, err := Convert(info, tables, opt)
			require.Error(t, err)
			require.Empty(t, p.TriggerSQL, "unsupported expressions must not produce a partially executable trigger")
			if tc.name != "optimizer_hint" { // Lexical rejection precedes statement position tracking.
				require.Contains(t, err.Error(), "第")
			}
		})
	}
}

func TestBareUUIDAssignmentRejectsQueriesAndPartialPatterns(t *testing.T) {
	for _, rhs := range []string{
		"(SELECT UUID() FROM jwjd_ajspxx)",
		"(SELECT UUID() FROM DUAL)",
		"(SELECT UUID() WHERE TRUE)",
		"(SELECT MAX(UUID()))",
		"(SELECT UUID() GROUP BY 1)",
		"(SELECT UUID() HAVING TRUE)",
		"(SELECT UUID() ORDER BY 1)",
		"(SELECT UUID() LIMIT 1)",
		"(SELECT UUID() UNION SELECT UUID())",
		"(SELECT UUID() AS id)",
		"(SELECT UUID(), 1)",
		"(SELECT DISTINCT UUID())",
		"(SELECT ALL UUID())",
		"(SELECT SQL_NO_CACHE UUID())",
		"(SELECT SQL_CACHE UUID())",
		"(SELECT SQL_SMALL_RESULT UUID())",
		"(SELECT /*+ MAX_EXECUTION_TIME(1) */ UUID())",
		"UUID(1)",
		"Src.UUID()",
		"CONCAT(UUID(), 'x')",
		"IFNULL(UUID(), 'x')",
		"CASE WHEN TRUE THEN UUID() ELSE 'x' END",
		"UUID() = 'x'",
		"UUID() + 1",
	} {
		t.Run(rhs, func(t *testing.T) {
			info, tables, opt := uuidAssignmentFixture("BEGIN SET NEW.Amount=1; SET NEW.RowGuid=" + rhs + "; END")
			tables[info.TableName].Columns[0].Length = 36
			p, err := Convert(info, tables, opt)
			require.Error(t, err)
			require.Empty(t, p.TriggerSQL)
		})
	}
}

func TestUUIDAssignmentDoesNotEnableOtherExpressionContexts(t *testing.T) {
	for _, rhs := range []string{compactUUIDSourceExpression, "UUID()"} {
		for _, body := range []string{
			"BEGIN DECLARE v VARCHAR(36) DEFAULT " + rhs + "; SET NEW.RowGuid=v; END",
			"INSERT INTO Audit(RowGuid) VALUES(" + rhs + ")",
			"BEGIN IF " + rhs + " = 'x' THEN SET NEW.Amount=1; END IF; END",
			"SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT=" + rhs,
		} {
			t.Run(body, func(t *testing.T) {
				info, tables, opt := uuidAssignmentFixture(body)
				tables["Audit"] = &source.TableDDLInfo{TableName: "Audit", Columns: []source.ColumnInfo{{Name: "RowGuid", DataType: "varchar", Length: 36}}}
				p, err := Convert(info, tables, opt)
				require.Error(t, err)
				require.Empty(t, p.TriggerSQL)
			})
		}
	}
}

func TestUUIDAssignmentRetainsTargetAndSQLModeValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*source.TriggerInfo, map[string]*source.TableDDLInfo)
	}{
		{"after_insert", func(info *source.TriggerInfo, _ map[string]*source.TableDDLInfo) { info.Timing = "AFTER" }},
		{"before_delete", func(info *source.TriggerInfo, _ map[string]*source.TableDDLInfo) { info.Event = "DELETE" }},
		{"numeric_column", func(info *source.TriggerInfo, tables map[string]*source.TableDDLInfo) {
			tables[info.TableName].Columns[0].DataType = "int"
		}},
		{"auto_increment", func(info *source.TriggerInfo, tables map[string]*source.TableDDLInfo) {
			tables[info.TableName].Columns[0].Extra = "auto_increment"
		}},
		{"generated_column", func(info *source.TriggerInfo, tables map[string]*source.TableDDLInfo) {
			tables[info.TableName].Columns[0].Generated = "UUID()"
		}},
		{"unsupported_sql_mode", func(info *source.TriggerInfo, _ map[string]*source.TableDDLInfo) { info.SQLMode = "PIPES_AS_CONCAT" }},
	} {
		for _, rhs := range []string{compactUUIDSourceExpression, "UUID()"} {
			t.Run(tc.name+"/"+rhs, func(t *testing.T) {
				info, tables, opt := uuidAssignmentFixture("SET NEW.RowGuid=" + rhs)
				tables[info.TableName].Columns[0].Length = 36
				tc.change(&info, tables)
				p, err := Convert(info, tables, opt)
				require.Error(t, err)
				require.Empty(t, p.TriggerSQL)
			})
		}
	}
	for _, mode := range []string{"", "STRICT_TRANS_TABLES", "ANSI_QUOTES,NO_BACKSLASH_ESCAPES,IGNORE_SPACE"} {
		t.Run("supported_mode/"+mode, func(t *testing.T) {
			info, tables, opt := uuidAssignmentFixture("SET NEW.RowGuid=" + compactUUIDSourceExpression)
			info.SQLMode = mode
			p, err := Convert(info, tables, opt)
			require.NoError(t, err)
			require.NoError(t, ValidatePlan(p))
			require.Equal(t, mode, p.Source.SQLMode)
		})
	}
	info, tables, opt := uuidAssignmentFixture(`SET NEW.RowGuid=REPLACE(UUID(), "-", "")`)
	info.SQLMode = "ANSI_QUOTES"
	p, err := Convert(info, tables, opt)
	require.Error(t, err, "ANSI_QUOTES identifiers must not be mistaken for string literals")
	require.Empty(t, p.TriggerSQL)
}

func TestUUIDAssignmentPreservesExistingV1Program(t *testing.T) {
	info, tables, opt := triggerFixture("SET NEW.Amount=NEW.Amount+1")
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.Equal(t, 1, p.Version)
	require.Equal(t, "CREATE TRIGGER \"DEST\".\"ORDERAUDIT\" BEFORE UPDATE ON \"DEST\".\"ORDERDATA\" FOR EACH ROW\nBEGIN\n  /* dbgold:mysql-dameng-trigger:v1 */\n:NEW.\"AMOUNTVALUE\" := (:NEW.\"AMOUNTVALUE\" + 1);\nEND;", p.TriggerSQL)
	require.NoError(t, ValidatePlan(p))
}

func TestUUIDAssignmentPreservesExistingCompactV1Program(t *testing.T) {
	info, tables, opt := uuidAssignmentFixture("SET NEW.RowGuid=" + compactUUIDSourceExpression)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.Equal(t, 1, p.Version)
	require.Equal(t, "CREATE TRIGGER \"DEST\".\"TRI_JWJD_AJSPXX_ROWGUID\" BEFORE INSERT ON \"DEST\".\"CASEDATA\" FOR EACH ROW\nBEGIN\n  /* dbgold:mysql-dameng-trigger:v1 */\n:NEW.\"ROWTOKEN\" := LOWER(RAWTOHEX(SYS_GUID()));\nEND;", p.TriggerSQL)
	require.NoError(t, ValidatePlan(p))
}
