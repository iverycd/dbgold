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
	ID                   string             `json:"id"`
	Kind                 string             `json:"kind"`
	Name                 string             `json:"name"`
	Schema               string             `json:"schema"`
	Table                string             `json:"table,omitempty"`
	ParentID             string             `json:"parent_id,omitempty"`
	Managed              bool               `json:"managed,omitempty"`         // recreated with its parent
	Generated            bool               `json:"generated,omitempty"`       // catalog proves generated name
	ConstraintType       string             `json:"constraint_type,omitempty"` // original P/U/R/C kind
	ConstraintNoValidate bool               `json:"constraint_no_validate,omitempty"`
	EventTrigger         bool               `json:"event_trigger,omitempty"` // schema/database event trigger
	Materialized         *MaterializedState `json:"materialized,omitempty"`
	Status               string             `json:"status,omitempty"`
	Dependencies         []string           `json:"dependencies"`
	External             []Reference        `json:"external,omitempty"`
	Units                []Unit             `json:"units"`
	DropSQL              string             `json:"drop_sql,omitempty"`
	CompileSQL           string             `json:"compile_sql,omitempty"`
}

// MaterializedState records the explicit logical baseline action. A physical
// ROWID cannot survive copying into a new database; only direct ROWID output
// columns may be rebased, after all ordinary values and multiplicities match.
type MaterializedState struct {
	Query           string                   `json:"query"`
	RefreshMethod   string                   `json:"refresh_method"`
	RefreshRule     string                   `json:"refresh_rule"`
	Staleness       string                   `json:"staleness"`
	Baseline        bool                     `json:"baseline"`
	SnapshotColumns []string                 `json:"snapshot_columns"`
	RowIDColumns    []int                    `json:"rowid_columns,omitempty"` // zero-based direct output ordinals
	LastRefresh     string                   `json:"last_refresh,omitempty"`  // source native DATETIME(6), no wall-clock reinterpretation
	History         *MaterializedHistoryPlan `json:"history,omitempty"`       // present only on the group's leader
}

// MaterializedHistoryPlan is an immutable, credential-free group recipe. Staging
// tables contain the original copied rows and are never exchanged or refreshed;
// each attempt makes new disposable exchange buffers from them. The engine's own
// COMPLETE refresh establishes target-local watermarks before the old snapshots
// and their still-pending change logs are restored with native partition exchange
// or online redefinition of protected HASH storage.
type MaterializedHistoryPlan struct {
	Version   int                       `json:"version"`
	Schema    string                    `json:"schema"`
	Views     []MaterializedHistoryView `json:"views"`
	Logs      []MaterializedHistoryLog  `json:"logs"`
	RowIDMaps []MaterializedRowIDMap    `json:"rowid_maps,omitempty"`
}

// MaterializedRowIDMap is populated alongside each base-table insert, so a
// source physical address is paired with the exact inserted target row even
// when the table has no key or contains duplicate values.
type MaterializedRowIDMap struct {
	BaseTable string `json:"base_table"`
	Table     string `json:"table"`
}

type MaterializedHistoryView struct {
	Current         bool                          `json:"current,omitempty"`
	Strategy        string                        `json:"strategy,omitempty"`
	Redefinition    *MaterializedRedefinitionPlan `json:"redefinition,omitempty"`
	ObjectID        string                        `json:"object_id"`
	Name            string                        `json:"name"`
	Table           string                        `json:"table"`
	Staging         string                        `json:"staging"`
	SourceRefresh   string                        `json:"source_refresh"`
	NativeStaleness string                        `json:"native_staleness"`
	TouchSQL        []string                      `json:"touch_sql,omitempty"`
	EmptyTouch      *MaterializedEmptyTouch       `json:"empty_touch,omitempty"`
	Query           string                        `json:"query"`
	Columns         []Column                      `json:"columns"`
	RowIDColumns    []int                         `json:"rowid_columns,omitempty"`
	RowIDTables     []string                      `json:"rowid_tables,omitempty"` // aligned with RowIDColumns, catalog-proved base owners
	Buffer          MaterializedExchangeBuffer    `json:"buffer"`
	Buffers         []MaterializedExchangeBuffer  `json:"buffers,omitempty"` // original partitioned storage, one buffer per leaf
}

type MaterializedRedefinitionPlan struct {
	WorkingTable string `json:"working_table"`
	SwapName     string `json:"swap_name"`
	CreateSQL    string `json:"create_sql"`
}

type MaterializedHistoryLog struct {
	ObjectID     string                     `json:"object_id"`
	BaseTable    string                     `json:"base_table"`
	Table        string                     `json:"table"`
	Staging      string                     `json:"staging"`
	Columns      []Column                   `json:"columns"`
	IdentityNext int64                      `json:"identity_next"`
	Buffer       MaterializedExchangeBuffer `json:"buffer"`
}

// Every statement is frozen from native metadata before target changes. The
// partitioned buffer is legal user storage for an unpartitioned MV/MLOG table;
// original partitioned MV storage instead uses a matching buffer for each leaf.
type MaterializedExchangeBuffer struct {
	Name           string   `json:"name"`
	Partition      string   `json:"partition"`
	CreateSQL      string   `json:"create_sql"`
	Indexes        []string `json:"indexes"`
	TablePartition string   `json:"table_partition,omitempty"`
	Subpartition   bool     `json:"subpartition,omitempty"`
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
	Shadow   bool   `json:"shadow,omitempty"` // durable extra snapshot input; validate the actual bound storage
	Name     string `json:"name"`
	// SourceName differs only for an MV snapshot copied into legal prebuilt storage.
	SourceName string   `json:"source_name,omitempty"`
	Schema     string   `json:"schema"`
	Columns    []Column `json:"columns"`
	Rows       int64    `json:"rows"`
	Restore    []Unit   `json:"restore,omitempty"`
	// CaptureRowID adds one cursor-only value after the writable columns. The
	// target persists its mapping in RowIDMapping in the same insert transaction.
	CaptureRowID bool   `json:"capture_rowid,omitempty"`
	RowIDMapping string `json:"rowid_mapping,omitempty"`
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
