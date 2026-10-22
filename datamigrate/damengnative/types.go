// Package damengnative is the credential-free contract for same-engine DM8
// migrations. It deliberately does not depend on the cross-vendor dialects.
package damengnative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type Profile struct {
	Version  string            `json:"version"`
	Instance string            `json:"instance"`
	Database string            `json:"database_identity"`
	Schema   string            `json:"schema"`
	Settings map[string]string `json:"settings"`
}

type Issue struct {
	ObjectID string `json:"object_id,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name,omitempty"`
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
}

type Unit struct {
	SQL   string `json:"sql"`
	Phase string `json:"phase"` // before_data | after_data | restore
	// DBLink credentials live only in memory. Reloaded plans obtain the exact
	// frozen definition from the source, never from a user-editable SQL draft.
	SourceSQLHash string `json:"source_sql_hash,omitempty"`
	executableSQL string
}

type Object struct {
	ID             string             `json:"id"`
	Kind           string             `json:"kind"`
	Name           string             `json:"name"`
	Schema         string             `json:"schema"`
	Table          string             `json:"table,omitempty"`
	ParentID       string             `json:"parent_id,omitempty"`
	Managed        bool               `json:"managed,omitempty"`         // recreated with its parent
	Generated      bool               `json:"generated,omitempty"`       // catalog proves generated name
	ConstraintType string             `json:"constraint_type,omitempty"` // original P/U/R/C kind
	EventTrigger   bool               `json:"event_trigger,omitempty"`   // schema/database event trigger
	Materialized   *MaterializedState `json:"materialized,omitempty"`
	Status         string             `json:"status,omitempty"`
	Dependencies   []string           `json:"dependencies"`
	External       []Reference        `json:"external,omitempty"`
	Units          []Unit             `json:"units"`
	DropSQL        string             `json:"drop_sql,omitempty"`
	CompileSQL     string             `json:"compile_sql,omitempty"`
}

// MaterializedState records the explicit logical baseline action. A physical
// ROWID cannot survive copying into a new database; only direct ROWID output
// columns may be rebased, after all ordinary values and multiplicities match.
type MaterializedState struct {
	Query           string   `json:"query"`
	RefreshMethod   string   `json:"refresh_method"`
	RefreshRule     string   `json:"refresh_rule"`
	Staleness       string   `json:"staleness"`
	Baseline        bool     `json:"baseline"`
	SnapshotColumns []string `json:"snapshot_columns"`
	RowIDColumns    []int    `json:"rowid_columns,omitempty"` // zero-based direct output ordinals
}

type Reference struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
}

type Column struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`
	SQLType    string `json:"sql_type"`
	TypeSchema string `json:"type_schema,omitempty"`
	TypeName   string `json:"type_name,omitempty"`
	Writable   bool   `json:"writable"`
	Identity   bool   `json:"identity,omitempty"`
	Precision  int64  `json:"precision,omitempty"`
	Scale      int64  `json:"scale,omitempty"`
}

type Table struct {
	ObjectID string `json:"object_id"`
	Name     string `json:"name"`
	// SourceName differs only for an MV snapshot copied into legal prebuilt storage.
	SourceName string   `json:"source_name,omitempty"`
	Schema     string   `json:"schema"`
	Columns    []Column `json:"columns"`
	Rows       int64    `json:"rows"`
	Restore    []Unit   `json:"restore,omitempty"`
}

type Snapshot struct {
	Profile Profile  `json:"profile"`
	Objects []Object `json:"objects"`
	Tables  []Table  `json:"tables"`
	Issues  []Issue  `json:"issues"`
}

type Scan interface {
	Next() bool
	Values() ([]any, error)
	Err() error
	Close() error
}

type Reader interface {
	SnapshotDamengNative(context.Context) (*Snapshot, error)
	OpenDamengNativeScan(context.Context, Table) (Scan, error)
}

type Writer interface {
	InspectDamengNative(context.Context) (*Snapshot, error)
	ExecuteDamengNative(context.Context, string) error
	CopyDamengNativeBatch(context.Context, Table, [][]any) error
	VerifyDamengNative(context.Context, *Snapshot) ([]Issue, error)
}

type Preflight struct {
	Kind        string    `json:"kind"`
	Ready       bool      `json:"ready"`
	Source      Profile   `json:"source"`
	Target      Profile   `json:"target"`
	Objects     []Object  `json:"objects"`
	Conflicts   []Object  `json:"conflicts"`
	Drops       []Object  `json:"drops"`
	Issues      []Issue   `json:"issues"`
	Fingerprint string    `json:"fingerprint"`
	Snapshot    *Snapshot `json:"-"`
}

func Quote(name string) string             { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func Qualified(schema, name string) string { return Quote(schema) + "." + Quote(name) }
func ObjectID(schema, kind, name string) string {
	return Fingerprint([]string{schema, kind, name})[:32]
}
func Fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func HasErrors(issues []Issue) bool {
	for _, issue := range issues {
		if issue.Severity != "warning" {
			return true
		}
	}
	return false
}

func NormalizeKind(kind string) string {
	kind = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(kind)), " ", "_")
	switch kind {
	case "dblink", "database_link":
		return "db_link"
	case "package_spec", "pkg_spec":
		return "package"
	case "pkg_body":
		return "package_body"
	case "class_head", "type_spec":
		return "class"
	}
	return kind
}
