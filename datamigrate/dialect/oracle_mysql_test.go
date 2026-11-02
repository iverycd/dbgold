package dialect

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/pingcap/tidb/pkg/parser"
)

func oracleMySQLTestInfo(count int, length int64) *source.TableDDLInfo {
	info := &source.TableDDLInfo{TableName: "WideTable"}
	for i := 0; i < count; i++ {
		info.Columns = append(info.Columns, source.ColumnInfo{Name: fmt.Sprintf("Column%d", i), DataType: "VARCHAR2", Length: length, IsNullable: true})
	}
	return info
}

func TestOracleMySQLWholeTableRowFit(t *testing.T) {
	for _, version := range []MySQLVersion{{Major: 5, Minor: 7, Patch: 44}, {Major: 8, Minor: 0, Patch: 15}, {Major: 8, Minor: 0, Patch: 36}} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			info := oracleMySQLTestInfo(9, 2000)
			original := append([]source.ColumnInfo(nil), info.Columns...)
			p, err := NewMySQL().PlanOracleMySQLTable("Target", info, OracleMySQLOptions{Version: version, ProtectedColumns: map[string]bool{"Column0": true}}, strings.ToLower)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(info.Columns, original) {
				t.Fatal("source metadata was modified")
			}
			if strings.Join(p.PromotedColumns, ",") != "Column1" || p.Columns[1].DataType != "longtext" || p.DeclaredRowBytes > 65535 || len(p.Warnings) != 1 {
				t.Fatalf("plan=%+v", p)
			}
			ddl := p.Statements[1].SQL
			for _, expected := range []string{"CREATE TABLE `Target`.`widetable`", "`column0` varchar(2000)", "`column1` longtext", "ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4"} {
				if !strings.Contains(ddl, expected) {
					t.Fatalf("missing %q in %s", expected, ddl)
				}
			}
			if strings.Contains(ddl, "CHECK (CHAR_LENGTH(`column1`) <= 2000)") != version.Checks() {
				t.Fatal(ddl)
			}
			if !version.Checks() && !strings.Contains(p.Warnings[0].Reason, "放宽原 2000 字符上限") {
				t.Fatal(p.Warnings)
			}
			if _, err := parser.New().ParseOneStmt(ddl, "", ""); err != nil {
				t.Fatal(err, ddl)
			}
		})
	}
}

func TestOracleMySQLPromotionGuards(t *testing.T) {
	for _, scenario := range []string{"key", "unknown_index", "default", "unsupported_default", "null_default", "quoted_null", "all_keys", "extended_varchar"} {
		t.Run(scenario, func(t *testing.T) {
			info := oracleMySQLTestInfo(9, 2000)
			opt := OracleMySQLOptions{Version: MySQLVersion{Major: 8, Minor: 0, Patch: 36}, ProtectedColumns: map[string]bool{}}
			want, failure := "Column0", false
			switch scenario {
			case "key":
				opt.ProtectedColumns["Column0"], want = true, "Column1"
			case "unknown_index":
				opt.BlockPromotion, failure = true, true
			case "default", "unsupported_default", "quoted_null":
				raw := "'abc'"
				if scenario == "unsupported_default" {
					raw = "my_function()"
				} else if scenario == "quoted_null" {
					raw = "'NULL'"
				}
				info.Columns[0].Default, want = &raw, "Column1"
			case "null_default":
				raw := "(( NULL ))"
				info.Columns[0].Default = &raw
			case "all_keys":
				for _, c := range info.Columns {
					opt.ProtectedColumns[c.Name] = true
				}
				failure = true
			case "extended_varchar":
				info = oracleMySQLTestInfo(1, 20000)
			}
			p, err := NewMySQL().PlanOracleMySQLTable("t", info, opt, nil)
			if (err != nil) != failure {
				t.Fatalf("err=%v plan=%+v", err, p)
			}
			if !failure && strings.Join(p.PromotedColumns, ",") != want {
				t.Fatal(p.PromotedColumns)
			}
			if scenario == "unsupported_default" && (p.Columns[0].Default != nil || len(p.Warnings) != 2) {
				t.Fatal("omitted source default lost its promotion protection", p)
			}
		})
	}
}

func TestOracleMySQLVersionIsRequestedOnlyWhenNeeded(t *testing.T) {
	if _, err := NewMySQL().PlanOracleMySQLTable("t", oracleMySQLTestInfo(1, 100), OracleMySQLOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMySQL().PlanOracleMySQLTable("t", oracleMySQLTestInfo(9, 2000), OracleMySQLOptions{}, nil); !errors.Is(err, ErrOracleMySQLVersionRequired) {
		t.Fatal(err)
	}
}

func TestOracleMySQLColumnNamesAreAlwaysCaseInsensitive(t *testing.T) {
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{
		{Name: "Foo", DataType: "NUMBER"}, {Name: "foo", DataType: "NUMBER"},
	}}
	for _, naming := range []NameFunc{nil, strings.ToLower, strings.ToUpper} {
		p, err := NewMySQL().PlanOracleMySQLTable("t", info, OracleMySQLOptions{}, naming)
		if err == nil || p != nil || !strings.Contains(err.Error(), "冲突") {
			t.Fatal(p, err)
		}
	}
}

func TestOracleMySQLDefaultsPreserveNullabilityAndLiteralCategories(t *testing.T) {
	for _, tc := range []struct {
		name, raw, sourceType, targetDefault string
		nullable, warning                    bool
	}{
		{"absent", "", "NUMBER", "", false, false},
		{"null_not_null", "NULL", "NUMBER", "", false, false},
		{"wrapped_null_not_null", "((NULL))", "NUMBER", "", false, false},
		{"oracle_empty_string", "''", "NUMBER", "", false, false},
		{"oracle_national_empty", "N''", "NUMBER", "", false, false},
		{"nullable_null", "NULL", "NUMBER", "NULL", true, false},
		{"zero", "0", "NUMBER", "0", false, false},
		{"negative", "(-1.5)", "NUMBER", "-1.5", false, false},
		{"scientific", "1e3", "NUMBER", "1e3", false, false},
		{"numeric_string", "'12.5'", "NUMBER", "12.5", false, false},
		{"quoted_null", "'NULL'", "VARCHAR2", "'NULL'", false, false},
		{"quoted_current_time", "'SYSDATE'", "VARCHAR2", "'SYSDATE'", false, false},
		{"quoted_number", "'0'", "VARCHAR2", "'0'", false, false},
		{"quote_escape", "'a''b'", "VARCHAR2", "'a''b'", false, false},
		{"backslash", `'a\b'`, "VARCHAR2", "_utf8mb4 X'615c62'", false, false},
		{"national", "N'中文'", "VARCHAR2", "'中文'", false, false},
		{"unsupported_function", "SYS_GUID()", "NUMBER", "", false, true},
		{"unsupported_nullable", "TO_NUMBER('1')", "NUMBER", "", true, true},
		{"unsupported_identifier", "SEQ_A.NEXTVAL", "NUMBER", "", false, true},
		{"wrong_time_type", "SYSDATE", "NUMBER", "", false, true},
		{"datetime_now", "SYSDATE", "DATE", "CURRENT_TIMESTAMP", false, false},
		{"timestamp_now", "CURRENT_TIMESTAMP", "TIMESTAMP(6)", "CURRENT_TIMESTAMP(6)", false, false},
		{"date_literal", "'2024-02-29 12:30:01'", "DATE", "'2024-02-29 12:30:01'", false, false},
		{"typed_date_literal", "DATE '2024-02-29'", "DATE", "'2024-02-29'", false, false},
		{"typed_timestamp_literal", "TIMESTAMP '2024-02-29 12:30:01.123456'", "TIMESTAMP(6)", "'2024-02-29 12:30:01.123456'", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			c := source.ColumnInfo{Name: "OrderNumber", DataType: tc.sourceType, Length: 100, Default: &raw, IsNullable: tc.nullable}
			if tc.name == "absent" {
				c.Default = nil
			}
			info := &source.TableDDLInfo{TableName: "FRAME_APP_CLASS", Columns: []source.ColumnInfo{c}}
			p, err := NewMySQL().PlanOracleMySQLTable("target", info, OracleMySQLOptions{}, strings.ToLower)
			if err != nil {
				t.Fatal(err)
			}
			ddl := p.Statements[1].SQL
			if !tc.nullable && !strings.Contains(ddl, " NOT NULL") || !tc.nullable && strings.Contains(ddl, "NOT NULL DEFAULT NULL") {
				t.Fatal(ddl)
			}
			if tc.targetDefault == "" {
				columnDDL := strings.Split(ddl, "\n")[1]
				if strings.Contains(columnDDL, " DEFAULT ") || p.Columns[0].Default != nil {
					t.Fatal(ddl, p.Columns)
				}
			} else if !strings.Contains(ddl, " DEFAULT "+tc.targetDefault) || p.Columns[0].Default == nil || *p.Columns[0].Default != tc.targetDefault {
				t.Fatal(ddl, p.Columns)
			}
			if (len(p.Warnings) > 0) != tc.warning {
				t.Fatal(p.Warnings)
			}
			if c.Default != nil && *c.Default != tc.raw {
				t.Fatal("source default mutated")
			}
			if _, err := parser.New().ParseOneStmt(ddl, "", ""); err != nil {
				t.Fatal(err, ddl)
			}
		})
	}
}

func TestOracleMySQLInvalidLiteralFailsBeforeDDL(t *testing.T) {
	for _, tc := range []struct {
		raw string
		col source.ColumnInfo
	}{
		{"'NULL'", source.ColumnInfo{DataType: "NUMBER"}},
		{"'abc'", source.ColumnInfo{DataType: "NUMBER"}},
		{"1e309", source.ColumnInfo{DataType: "NUMBER"}},
		{"(1e-400)", source.ColumnInfo{DataType: "NUMBER"}},
		{"40000", source.ColumnInfo{DataType: "NUMBER", Precision: 4}},
		{"12.34", source.ColumnInfo{DataType: "NUMBER", Precision: 3, Scale: 1}},
		{"'2024-02-30'", source.ColumnInfo{DataType: "DATE"}},
		{"'123456'", source.ColumnInfo{DataType: "VARCHAR2", Length: 5}},
	} {
		t.Run(tc.raw+tc.col.DataType, func(t *testing.T) {
			tc.col.Name, tc.col.Default = "BadColumn", &tc.raw
			if plan, err := NewMySQL().PlanOracleMySQLTable("t", &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{tc.col}}, OracleMySQLOptions{}, nil); err == nil || plan != nil {
				t.Fatal(plan, err)
			}
		})
	}
}

func TestOracleMySQLLOBDefaultVersionAndSourceIndependence(t *testing.T) {
	raw := "'value'"
	info := &source.TableDDLInfo{TableName: "T", Columns: []source.ColumnInfo{{Name: "Body", DataType: "CLOB", Default: &raw}}}
	if _, err := NewMySQL().PlanOracleMySQLTable("t", info, OracleMySQLOptions{}, nil); !errors.Is(err, ErrOracleMySQLVersionRequired) {
		t.Fatal(err)
	}
	for _, version := range []MySQLVersion{{Major: 5, Minor: 7, Patch: 44}, {Major: 8, Minor: 0, Patch: 36}} {
		p, err := NewMySQL().PlanOracleMySQLTable("t", info, OracleMySQLOptions{Version: version}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Statements[1].SQL, "DEFAULT ('value')") != version.Expressions() {
			t.Fatal(p.Statements)
		}
		if p.Columns[0].Default != nil {
			*p.Columns[0].Default = "'changed'"
		}
		if raw != "'value'" {
			t.Fatal("shared mutable default")
		}
	}
}
