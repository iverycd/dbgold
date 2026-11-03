package mysqldmtrigger

import (
	"strings"
	"testing"

	"dbgold/datamigrate/source"
	"github.com/stretchr/testify/require"
)

const implicitInsertRegressionBody = "begin if 1>0 then insert into epoint_zhdc.frame_attachstorage_ban values (1);end if;end"

func implicitInsertFixture(body string) (source.TriggerInfo, map[string]*source.TableDDLInfo, Options) {
	info := source.TriggerInfo{
		Schema: "epoint_zhdc", TableName: "frame_attachstorage", Name: "tg_frame_attachstorage_ban",
		Timing: "BEFORE", Event: "INSERT", Body: body, SQLMode: "STRICT_TRANS_TABLES",
		SourceDDL: "CREATE DEFINER=`root`@`%` TRIGGER `tg_frame_attachstorage_ban` BEFORE INSERT ON `frame_attachstorage` FOR EACH ROW " + body,
	}
	tables := map[string]*source.TableDDLInfo{
		"frame_attachstorage":     {TableName: "frame_attachstorage", Columns: []source.ColumnInfo{{Name: "Id", DataType: "int"}}},
		"frame_attachstorage_ban": {TableName: "frame_attachstorage_ban", Columns: []source.ColumnInfo{{Name: "BanFlag", DataType: "int"}}},
	}
	return info, tables, Options{Schema: "Dest"}
}

func TestImplicitInsertConvertsOriginalTrigger(t *testing.T) {
	info, tables, opt := implicitInsertFixture(implicitInsertRegressionBody)
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Equal(t, info, p.Source, "keep the complete original DDL and identifier case")
	require.Equal(t, []string{"FRAME_ATTACHSTORAGE", "FRAME_ATTACHSTORAGE_BAN"}, p.Dependencies)
	require.Contains(t, p.TriggerSQL, `INSERT INTO "DEST"."FRAME_ATTACHSTORAGE_BAN" ("BANFLAG") VALUES (1);`)
	require.NotContains(t, p.TriggerSQL, "epoint_zhdc.")
	require.NotContains(t, p.TriggerSQL, "DEFINER")
}

func TestImplicitInsertUsesSourceOrderAndMappings(t *testing.T) {
	info, tables, opt := implicitInsertFixture("INSERT INTO frame_attachstorage_ban VALUES (NEW.Id, 2)")
	tables["frame_attachstorage_ban"].Columns = []source.ColumnInfo{
		{Name: "Second", DataType: "int", Ordinal: 9},
		{Name: "First", DataType: "int", Ordinal: 1},
	}
	opt.TableNames = map[string]string{"frame_attachstorage_ban": "BanLog"}
	opt.ColumnNames = map[string]map[string]string{"frame_attachstorage_ban": {"Second": "RecordedSecond", "First": "RecordedFirst"}}
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, `INSERT INTO "DEST"."BANLOG" ("RECORDEDSECOND", "RECORDEDFIRST") VALUES (:NEW."ID", 2);`)
	require.Equal(t, "Second", p.SourceTables[0].Columns[0].Name)
	// The same resolved explicit columns must keep the previous successful SQL.
	info.Body = "INSERT INTO frame_attachstorage_ban (Second, First) VALUES (NEW.Id, 2)"
	info.SourceDDL = "CREATE TRIGGER tg_frame_attachstorage_ban BEFORE INSERT ON frame_attachstorage FOR EACH ROW " + info.Body
	explicit, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.Equal(t, explicit.TriggerSQL, p.TriggerSQL)
}

func TestImplicitInsertSkipsInvisibleColumns(t *testing.T) {
	info, tables, opt := implicitInsertFixture("INSERT INTO frame_attachstorage_ban VALUES (1, 2)")
	tables["frame_attachstorage_ban"].Columns = []source.ColumnInfo{
		{Name: "Before", DataType: "int"},
		{Name: "Hidden", DataType: "int", Extra: "invisible"},
		{Name: "After", DataType: "int"},
	}
	p, err := Convert(info, tables, opt)
	require.NoError(t, err)
	require.NoError(t, ValidatePlan(p))
	require.Contains(t, p.TriggerSQL, `("BEFORE", "AFTER") VALUES (1, 2);`)
	require.NotContains(t, p.TriggerSQL, `"HIDDEN"`)
}

func TestImplicitInsertRejectsUnsafeMetadataAndValues(t *testing.T) {
	dynamicDefault := "CURRENT_TIMESTAMP"
	for _, tc := range []struct {
		name, body, want string
		change           func(map[string]*source.TableDDLInfo)
	}{
		{"too many values", "INSERT INTO frame_attachstorage_ban VALUES (1, 2)", "列数和值数量不一致", nil},
		{"empty metadata", "INSERT INTO frame_attachstorage_ban VALUES (1)", "可见列元数据", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns = nil
		}},
		{"only invisible", "INSERT INTO frame_attachstorage_ban VALUES (1)", "可见列元数据", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns[0].Extra = "INVISIBLE"
		}},
		{"auto increment", "INSERT INTO frame_attachstorage_ban VALUES (NULL)", "自增列", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns[0].Extra = "auto_increment"
		}},
		{"generated expression", "INSERT INTO frame_attachstorage_ban VALUES (1)", "生成列", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns[0].Generated = "1+1"
		}},
		{"generated extra", "INSERT INTO frame_attachstorage_ban VALUES (1)", "生成列", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns[0].Extra = "VIRTUAL GENERATED"
		}},
		{"invisible generated", "INSERT INTO frame_attachstorage_ban VALUES (1)", "省略列 Hidden", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns = append(tables["frame_attachstorage_ban"].Columns, source.ColumnInfo{Name: "Hidden", DataType: "int", Extra: "STORED GENERATED INVISIBLE"})
		}},
		{"invisible dynamic default", "INSERT INTO frame_attachstorage_ban VALUES (1)", "省略列 Hidden", func(tables map[string]*source.TableDDLInfo) {
			tables["frame_attachstorage_ban"].Columns = append(tables["frame_attachstorage_ban"].Columns, source.ColumnInfo{Name: "Hidden", DataType: "timestamp", Default: &dynamicDefault, Extra: "DEFAULT_GENERATED INVISIBLE"})
		}},
		{"wrong type", "INSERT INTO frame_attachstorage_ban VALUES ('text')", "类型", nil},
		{"cross schema", "INSERT INTO other.frame_attachstorage_ban VALUES (1)", "跨源库引用", nil},
		{"missing table", "INSERT INTO missing VALUES (1)", "依赖表 missing 元数据不可用", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, tables, opt := implicitInsertFixture("BEGIN SET NEW.Id=1; " + tc.body + "; END")
			if tc.change != nil {
				tc.change(tables)
			}
			p, err := Convert(info, tables, opt)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, p.TriggerSQL, "reject the whole trigger, including earlier valid assignments")
			require.Contains(t, err.Error(), "第")
		})
	}
}

func TestInsertRestrictionsHaveSpecificDiagnostics(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"REPLACE INTO frame_attachstorage_ban VALUES (1)", "REPLACE"},
		{"INSERT IGNORE INTO frame_attachstorage_ban VALUES (1)", "INSERT IGNORE"},
		{"INSERT INTO frame_attachstorage_ban SET BanFlag=1", "INSERT SET"},
		{"INSERT HIGH_PRIORITY INTO frame_attachstorage_ban VALUES (1)", "优先级"},
		{"INSERT LOW_PRIORITY INTO frame_attachstorage_ban VALUES (1)", "优先级"},
		{"INSERT INTO frame_attachstorage_ban VALUES (1) ON DUPLICATE KEY UPDATE BanFlag=2", "ON DUPLICATE KEY UPDATE"},
		{"INSERT /*+ HASH_JOIN(frame_attachstorage_ban) */ INTO frame_attachstorage_ban VALUES (1)", "优化提示"},
		{"INSERT INTO frame_attachstorage_ban PARTITION (p0) VALUES (1)", "PARTITION"},
		{"INSERT INTO frame_attachstorage_ban SELECT 1", "INSERT SELECT 必须指定明确列清单"},
		{"INSERT INTO frame_attachstorage_ban VALUES (1), (2)", "仅支持单行 VALUES"},
		{"INSERT INTO frame_attachstorage_ban (BanFlag) VALUES (1), (2)", "多行 INSERT"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			info, tables, opt := implicitInsertFixture("BEGIN SET NEW.Id=1; " + tc.body + "; END")
			p, err := Convert(info, tables, opt)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, p.TriggerSQL)
			require.False(t, strings.Contains(err.Error(), "只支持明确列清单的 VALUES/SELECT"))
		})
	}
}
