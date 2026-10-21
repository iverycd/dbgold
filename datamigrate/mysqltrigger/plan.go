// Package mysqltrigger translates a deliberately restricted MySQL trigger subset.
package mysqltrigger

import "dbgold/datamigrate/source"

const ToolMarker = "dbgold:mysql-trigger:v1"

type Plan struct {
	Source                                                     source.TriggerInfo
	Schema, Table, Name, FunctionName, FunctionSQL, TriggerSQL string
	FunctionBody                                               string
	Dependencies                                               []string
	Tables                                                     []source.TableDDLInfo
	SourceTables                                               []source.TableDDLInfo
	LowerCaseNames                                             bool
	Owner                                                      string
}

func (p Plan) Statements() []string { return []string{p.FunctionSQL, p.TriggerSQL} }

type Options struct {
	Schema string
	Name   func(string) string
}
