package mysqldmtrigger

import (
	"fmt"
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
		{"uuid_bare", "UUID()"},
		{"uuid_scalar_select", "(SELECT UUID())"},
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

func TestUUIDAssignmentDoesNotEnableOtherExpressionContexts(t *testing.T) {
	for _, body := range []string{
		"BEGIN DECLARE v VARCHAR(32) DEFAULT " + compactUUIDSourceExpression + "; SET NEW.RowGuid=v; END",
		"INSERT INTO Audit(RowGuid) VALUES(" + compactUUIDSourceExpression + ")",
		"BEGIN IF " + compactUUIDSourceExpression + " = 'x' THEN SET NEW.Amount=1; END IF; END",
		"SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT=" + compactUUIDSourceExpression,
	} {
		t.Run(body, func(t *testing.T) {
			info, tables, opt := uuidAssignmentFixture(body)
			tables["Audit"] = &source.TableDDLInfo{TableName: "Audit", Columns: []source.ColumnInfo{{Name: "RowGuid", DataType: "varchar", Length: 32}}}
			p, err := Convert(info, tables, opt)
			require.Error(t, err)
			require.Empty(t, p.TriggerSQL)
		})
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
		t.Run(tc.name, func(t *testing.T) {
			info, tables, opt := uuidAssignmentFixture("SET NEW.RowGuid=" + compactUUIDSourceExpression)
			tc.change(&info, tables)
			p, err := Convert(info, tables, opt)
			require.Error(t, err)
			require.Empty(t, p.TriggerSQL)
		})
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
