// Package repair contains the durable, credential-free migration repair contract.
package repair

import (
	"context"
	"crypto/sha256"
	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/mysqltrigger"
	"dbgold/datamigrate/source"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
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
	Native            *damengnative.Object `json:"native,omitempty"`
	NativeFingerprint string               `json:"native_fingerprint,omitempty"`
	NativeTable       *damengnative.Table  `json:"native_table,omitempty"`
	MySQLPGTrigger    *mysqltrigger.Plan   `json:"mysql_pg_trigger,omitempty"`
	// Deprecated metadata is retained only to read old records; it does not affect execution status.
	Verification       *Verification        `json:"verification,omitempty"`
	ManualVerification *ManualVerification  `json:"manual_verification,omitempty"`
	OriginalStatus     string               `json:"original_status"`
	OriginalError      string               `json:"original_error"`
	ID                 string               `json:"id"`
	Kind               string               `json:"kind"`
	Name               string               `json:"name"`
	Table              string               `json:"table,omitempty"`
	SourceName         string               `json:"source_name,omitempty"`
	SourceTable        string               `json:"source_table,omitempty"`
	TargetName         string               `json:"target_name"`
	Schema             string               `json:"schema"`
	DDL                string               `json:"ddl"`
	SourceDDL          string               `json:"source_ddl,omitempty"` // Reference only; never an execution candidate.
	DiagnosticOnly     bool                 `json:"diagnostic_only,omitempty"`
	SuggestedDDL       string               `json:"suggested_ddl,omitempty"`
	Error              string               `json:"error,omitempty"`
	Status             string               `json:"status"`
	Units              []Unit               `json:"units"`
	Dependencies       []string             `json:"dependencies"`
	ColumnMapping      map[string]string    `json:"column_mapping,omitempty"`
	RefColumnMapping   map[string]string    `json:"ref_column_mapping,omitempty"`
	Columns            []source.ColumnInfo  `json:"columns,omitempty"`
	SourceColumns      []source.ColumnInfo  `json:"source_columns,omitempty"`
	Index              *source.IndexInfo    `json:"index,omitempty"`
	ForeignKey         *source.FKInfo       `json:"foreign_key,omitempty"`
	Sequence           *source.SequenceInfo `json:"sequence,omitempty"`
	Comment            *source.CommentInfo  `json:"comment,omitempty"`
	Legacy             bool                 `json:"legacy"`
	RequiresReview     bool                 `json:"requires_review"`
	DataStatus         string               `json:"data_status,omitempty"`
}

type Verification struct {
	Status  string `json:"status"` // succeeded, failed, unknown
	Message string `json:"message"`
}

type ManualVerification struct {
	ConfirmedBy uint      `json:"confirmed_by"`
	ConfirmedAt time.Time `json:"confirmed_at"`
	Evidence    string    `json:"evidence"`
	Fingerprint string    `json:"fingerprint"`
}

func (o Object) AllUnitsSucceeded() bool {
	if len(o.Units) == 0 {
		return false
	}
	for _, u := range o.Units {
		if u.Status != "succeeded" || strings.TrimSpace(u.SQL) == "" || u.Error != "" {
			return false
		}
	}
	return true
}

// NormalizeLegacyExecution upgrades the old verification-gated status using
// complete saved execution evidence. It never replays SQL or modifies history.
func (o *Object) NormalizeLegacyExecution() bool {
	if o.Status != "unverified" {
		return false
	}
	o.Status = "unknown"
	if !o.DiagnosticOnly && o.AllUnitsSucceeded() {
		o.Status = "succeeded"
		for i := range o.Units {
			o.Units[i].Selected = false
		}
	}
	return true
}

// CanEditDDL requires an object identity, not just a report category label.
func (o Object) CanEditDDL() bool {
	if o.Native != nil {
		return false
	}
	if o.DiagnosticOnly || strings.TrimSpace(o.Schema) == "" || strings.TrimSpace(o.TargetName) == "" {
		return false
	}
	switch o.Kind {
	case "table", "tables", "view", "views", "sequence", "sequences", "owner", "distribution":
		return true
	case "index", "indexes", "primary_key", "primary_keys", "primaryKeys", "unique", "foreign_key", "foreign_keys", "constraints", "comment", "comments":
		return o.Table != "" || o.DDL != "" || len(o.Units) > 0
	default:
		return false
	}
}

// Generated MySQL trigger programs can be retried as a complete immutable
// transaction, while remaining unavailable to the general SQL draft editor.
func (o Object) CanExecuteDDL() bool {
	if o.Native != nil {
		return o.TrustedNative()
	}
	return o.CanEditDDL() || (!o.DiagnosticOnly && o.Kind == "trigger" && o.MySQLPGTrigger != nil && o.Schema != "" && o.TargetName != "")
}

// TrustedNative binds the immutable server-side plan and every physical unit.
// A generic SQL draft can never acquire native execution authority.
func (o Object) TrustedNative() bool {
	if o.DiagnosticOnly || o.Native == nil || o.Native.Managed || o.Schema == "" || o.Schema != o.Native.Schema || o.TargetName != o.Native.Name || o.NativeFingerprint == "" || o.NativeFingerprint != damengnative.Fingerprint(o.Native) || len(o.Units) != len(o.Native.Units) {
		return false
	}
	for i, u := range o.Units {
		if u.SQL == "" || u.SQL != u.OriginalSQL || u.SQL != o.Native.Units[i].SQL {
			return false
		}
	}
	return len(o.Units) > 0
}

type RecoveryPreview struct {
	NativeTable     *damengnative.Table  `json:"native_table,omitempty"`
	NativeObject    *damengnative.Object `json:"native_object,omitempty"`
	NativeUnits     []Unit               `json:"native_units,omitempty"`
	NativeDisable   []damengnative.Unit  `json:"native_disable,omitempty"`
	NativeRestore   []damengnative.Unit  `json:"native_restore,omitempty"`
	ObjectID        string               `json:"object_id"`
	SourceTable     string               `json:"source_table"`
	TargetTable     string               `json:"target_table"`
	QualifiedTarget string               `json:"qualified_target"`
	SourceRows      int64                `json:"source_rows"`
	TargetRows      int64                `json:"target_rows"`
	ColumnMapping   map[string]string    `json:"column_mapping"`
	SourceColumns   []source.ColumnInfo  `json:"source_columns"`
	TargetColumns   []source.ColumnInfo  `json:"target_columns"`
	Related         []Object             `json:"related"`
	DataRequired    bool                 `json:"data_required"`
	Fingerprint     string               `json:"fingerprint"`
	Warnings        []string             `json:"warnings"`
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

// An observer sees SQL execution and read-only "reused" verification events;
// display SQL is never split to infer physical execution units.
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
