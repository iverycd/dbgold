package mysqltrigger

import (
	"reflect"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

const userEnableTriggerDDL = "CREATE DEFINER=`root`@`%` TRIGGER `update_enableuser` AFTER UPDATE ON `frame_user` FOR EACH ROW begin if (old.isenabled!=new.isenabled) then insert into changeuserinfo(userguid,loginid,displayname,operatedate,dealtype) values(new.userguid,new.loginid,new.displayname,now(),CONCAT('Frame_User表isenabled字段由【||',OLD.ISENABLED,'||】更新为【||',NEW.ISENABLED,'||】')); end if; end"

const userUSBKeyTriggerDDL = "CREATE DEFINER=`root`@`%` TRIGGER `update_isepoint` AFTER UPDATE ON `frame_user_extendinfo` FOR EACH ROW begin if (old.usbkey!=new.usbkey) then insert into changeuserinfo(userguid,loginid,displayname,operatedate,dealtype) select userguid,loginid,displayname,now(),CONCAT('Frame_User_ExtendInfo表usbkey字段由【||',OLD.USBKEY,'||】更新为【||',NEW.USBKEY,'||】') from `sxk`.frame_user where userguid = new.userguid; end if; end"

func userTables() map[string]*source.TableDDLInfo {
	return map[string]*source.TableDDLInfo{
		"frame_user":            {TableName: "frame_user", Columns: []source.ColumnInfo{{Name: "userguid", DataType: "varchar", Length: 50}, {Name: "loginid", DataType: "varchar", Length: 50}, {Name: "displayname", DataType: "varchar", Length: 50}, {Name: "isenabled", DataType: "int"}}},
		"frame_user_extendinfo": {TableName: "frame_user_extendinfo", Columns: []source.ColumnInfo{{Name: "userguid", DataType: "varchar", Length: 50}, {Name: "usbkey", DataType: "varchar", Length: 100, IsNullable: true}}},
		"changeuserinfo":        {TableName: "changeuserinfo", Columns: []source.ColumnInfo{{Name: "id", DataType: "bigint", Extra: "auto_increment"}, {Name: "userguid", DataType: "varchar", Length: 50}, {Name: "loginid", DataType: "varchar", Length: 50}, {Name: "displayname", DataType: "varchar", Length: 50}, {Name: "operatedate", DataType: "datetime"}, {Name: "dealtype", DataType: "varchar", Length: 500}}},
	}
}

func TestUserEnableAndUSBKeyBusinessTriggers(t *testing.T) {
	for _, flagType := range []string{"int", "varchar"} {
		t.Run(flagType, func(t *testing.T) {
			tables := userTables()
			tables["frame_user"].Columns[3].DataType = flagType
			meta := characterFixture("sxk", tables)
			for _, tc := range []struct {
				ddl, name, table string
				dependencies     []string
			}{
				{userEnableTriggerDDL, "update_enableuser", "frame_user", []string{"changeuserinfo", "frame_user"}},
				{userUSBKeyTriggerDDL, "update_isepoint", "frame_user_extendinfo", []string{"changeuserinfo", "frame_user", "frame_user_extendinfo"}},
			} {
				i := source.TriggerInfo{Schema: "sxk", Name: tc.name, TableName: tc.table, Timing: "AFTER", Event: "UPDATE", Body: tc.ddl[strings.Index(tc.ddl, "begin"):], SourceDDL: tc.ddl, Definer: "root@%", ActionOrder: 1}
				p, err := Convert(i, tables, Options{Schema: "TargetSchema", Name: strings.ToLower, CharacterMetadata: meta})
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				if p.Source.SourceDDL != tc.ddl || p.Source.Body != i.Body {
					t.Fatal("original business definition changed")
				}
				if !reflect.DeepEqual(p.Dependencies, tc.dependencies) {
					t.Fatalf("%s deps %v", tc.name, p.Dependencies)
				}
				if !strings.Contains(p.FunctionBody, `INSERT INTO "TargetSchema"."changeuserinfo"`) || !strings.Contains(p.FunctionBody, " || ") || !strings.Contains(p.FunctionBody, "更新为【||") {
					t.Fatal(p.FunctionBody)
				}
				if tc.name == "update_isepoint" {
					if !strings.Contains(p.FunctionBody, `FROM "TargetSchema"."frame_user" WHERE `) || strings.Contains(p.FunctionBody, "IS DISTINCT FROM") {
						t.Fatal("query mapping or NULL semantics changed")
					}
					if p.CharacterMetadata == nil || len(p.CharacterMetadata.Columns) != 3 {
						t.Fatalf("missing USBKey and userguid collation dependencies: %+v", p.CharacterMetadata)
					}
				}
				if err = ValidatePlan(p); err != nil {
					t.Fatalf("frozen %s: %v", tc.name, err)
				}
			}
		})
	}
}
