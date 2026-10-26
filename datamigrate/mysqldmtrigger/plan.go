// Package mysqldmtrigger translates statically understood MySQL row triggers
// into complete Dameng DMSQL programs. It never executes SQL.
package mysqldmtrigger

import (
	"encoding/json"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

const CurrentVersion = 1
const ToolMarker = "dbgold:mysql-dameng-trigger:v1"

type Capabilities struct {
	CompatibleMode                          string
	EmptyStringIsNull, ConcatNullYieldsNull bool
}

type Options struct {
	Schema      string
	TableNames  map[string]string
	ColumnNames map[string]map[string]string
	Predecessor string
	// Read-only predecessor proofs; these SQL strings are never execution units.
	PredecessorSQLs map[string]string
	// Optional existing group members, compared to catalogs but never executed.
	PeerSQLs          map[string]string
	CharacterMetadata *source.TriggerCharacterMetadata
	Capabilities      Capabilities
}

type Plan struct {
	Version                                      int
	Source                                       source.TriggerInfo
	Schema, Table, Name, TriggerSQL, Predecessor string
	Dependencies                                 []string
	SourceTables                                 []source.TableDDLInfo
	Options                                      Options
}

func (p Plan) Statements() []string { return []string{p.TriggerSQL} }

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ValidatePlan binds the complete generated program to its frozen source and
// mappings; SQL text and an embedded marker alone never confer execution trust.
func ValidatePlan(p Plan) error {
	if p.Version != CurrentVersion {
		return fmt.Errorf("不支持 MySQL→达梦触发器计划版本 %d", p.Version)
	}
	tables := map[string]*source.TableDDLInfo{}
	for i := range p.SourceTables {
		t := &p.SourceTables[i]
		if tables[t.TableName] != nil {
			return fmt.Errorf("重复的触发器源表快照 %s", t.TableName)
		}
		tables[t.TableName] = t
	}
	want, err := Convert(p.Source, tables, p.Options)
	if err != nil {
		return err
	}
	gotJSON, _ := json.Marshal(p)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		return fmt.Errorf("达梦触发器计划与冻结源定义或名称映射不一致")
	}
	return nil
}
