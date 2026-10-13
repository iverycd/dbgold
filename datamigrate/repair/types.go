// Package repair contains the durable, credential-free migration repair contract.
package repair

import (
	"context"
	"crypto/sha256"
	"dbgold/datamigrate/source"
	"encoding/hex"
	"encoding/json"
)

type Unit struct {
	ID          string `json:"id"`
	SQL         string `json:"sql"`
	OriginalSQL string `json:"original_sql"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Selected    bool   `json:"selected"`
	Destructive bool   `json:"destructive"`
}

type Object struct {
	OriginalStatus   string               `json:"original_status"`
	OriginalError    string               `json:"original_error"`
	ID               string               `json:"id"`
	Kind             string               `json:"kind"`
	Name             string               `json:"name"`
	Table            string               `json:"table,omitempty"`
	SourceName       string               `json:"source_name,omitempty"`
	SourceTable      string               `json:"source_table,omitempty"`
	TargetName       string               `json:"target_name"`
	Schema           string               `json:"schema"`
	DDL              string               `json:"ddl"`
	SuggestedDDL     string               `json:"suggested_ddl,omitempty"`
	Error            string               `json:"error,omitempty"`
	Status           string               `json:"status"`
	Units            []Unit               `json:"units"`
	Dependencies     []string             `json:"dependencies"`
	ColumnMapping    map[string]string    `json:"column_mapping,omitempty"`
	RefColumnMapping map[string]string    `json:"ref_column_mapping,omitempty"`
	Columns          []source.ColumnInfo  `json:"columns,omitempty"`
	SourceColumns    []source.ColumnInfo  `json:"source_columns,omitempty"`
	Index            *source.IndexInfo    `json:"index,omitempty"`
	ForeignKey       *source.FKInfo       `json:"foreign_key,omitempty"`
	Sequence         *source.SequenceInfo `json:"sequence,omitempty"`
	Comment          *source.CommentInfo  `json:"comment,omitempty"`
	Legacy           bool                 `json:"legacy"`
	RequiresReview   bool                 `json:"requires_review"`
	DataStatus       string               `json:"data_status,omitempty"`
}

type Verification struct {
	Status  string `json:"status"` // succeeded, failed, unknown
	Message string `json:"message"`
}

type RecoveryPreview struct {
	ObjectID        string              `json:"object_id"`
	SourceTable     string              `json:"source_table"`
	TargetTable     string              `json:"target_table"`
	QualifiedTarget string              `json:"qualified_target"`
	SourceRows      int64               `json:"source_rows"`
	TargetRows      int64               `json:"target_rows"`
	ColumnMapping   map[string]string   `json:"column_mapping"`
	SourceColumns   []source.ColumnInfo `json:"source_columns"`
	TargetColumns   []source.ColumnInfo `json:"target_columns"`
	Related         []Object            `json:"related"`
	DataRequired    bool                `json:"data_required"`
	Fingerprint     string              `json:"fingerprint"`
	Warnings        []string            `json:"warnings"`
}

type RecoveryResult struct {
	DataStatus    string `json:"data_status"`
	Status        string `json:"status"`
	CommittedRows int64  `json:"committed_rows"`
	SourceRows    int64  `json:"source_rows"`
	TargetRows    int64  `json:"target_rows"`
	Error         string `json:"error,omitempty"`
}

func Hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// An observer sees actual execution units; display SQL is never split to infer them.
type Event struct {
	SQL    string
	Status string
	Error  string
}
type observerKey struct{}

func WithObserver(ctx context.Context, fn func(Event) error) context.Context {
	return context.WithValue(ctx, observerKey{}, fn)
}
func Observe(ctx context.Context, sql, status string, err error) error {
	if fn, ok := ctx.Value(observerKey{}).(func(Event) error); ok {
		e := Event{SQL: sql, Status: status}
		if err != nil {
			e.Error = err.Error()
		}
		return fn(e)
	}
	return nil
}
