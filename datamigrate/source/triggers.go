package source

import "context"

// TriggerInfo retains the source definition and spelling for audit and adaptation.
type TriggerInfo struct {
	Name, TableName, Schema, Timing, Event, Body, SourceDDL, SQLMode, Definer string
	ActionOrder                                                               int
	MetadataError                                                             string
}

// TriggerReader is optional: unsupported readers need not implement trigger migration.
type TriggerReader interface {
	GetTriggers(context.Context) ([]TriggerInfo, error)
}
