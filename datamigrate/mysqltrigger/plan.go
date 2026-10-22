// Package mysqltrigger translates a deliberately restricted MySQL trigger subset.
package mysqltrigger

import "dbgold/datamigrate/source"

const (
	ToolMarker       = "dbgold:mysql-trigger:v4"
	legacyToolMarker = "dbgold:mysql-trigger:v1"
	v2ToolMarker     = "dbgold:mysql-trigger:v2"
	v3ToolMarker     = "dbgold:mysql-trigger:v3"
	CurrentVersion   = 4
)

type Plan struct {
	Version                                                    int `json:",omitempty"`
	Source                                                     source.TriggerInfo
	Schema, Table, Name, FunctionName, FunctionSQL, TriggerSQL string
	FunctionBody                                               string
	Dependencies                                               []string
	Tables                                                     []source.TableDDLInfo
	SourceTables                                               []source.TableDDLInfo
	LowerCaseNames                                             bool
	Owner                                                      string
	CharacterMetadata                                          *source.TriggerCharacterMetadata `json:",omitempty"`
	RequiresTextSemantics                                      bool                             `json:",omitempty"`
	NativeUserVariables                                        bool                             `json:",omitempty"`
}

func (p Plan) Statements() []string { return []string{p.FunctionSQL, p.TriggerSQL} }

type Options struct {
	Schema              string
	Name                func(string) string
	CharacterMetadata   *source.TriggerCharacterMetadata
	NativeUserVariables bool
}
