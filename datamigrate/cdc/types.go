package cdc

import "time"

const CheckpointTableName = "__dbgold_cdc_checkpoint"

type Position struct {
	File string `json:"file"`
	Pos  uint32 `json:"position"`
	GTID string `json:"gtid"`
}

type TableInfo struct {
	Name            string            `json:"name"`
	Engine          string            `json:"engine"`
	Columns         []string          `json:"columns"`
	ColumnTypes     []string          `json:"column_types"`
	PrimaryKey      []int             `json:"primary_key_indexes"`
	AutoIncrement   []int             `json:"auto_increment_indexes"`
	LocatorStrategy string            `json:"locator_strategy"`
	LocatorIndex    string            `json:"locator_index,omitempty"`
	LocatorColumns  []string          `json:"locator_columns"`
	LocatorWarning  string            `json:"locator_warning,omitempty"`
	UniqueIndexes   []UniqueIndexInfo `json:"-"`
}

const LocatorStrategyVersion = 1

const (
	LocatorPrimaryKey = "primary_key"
	LocatorUniqueKey  = "unique_key"
	LocatorFullRow    = "full_row"
)

type UniqueIndexInfo struct {
	Name    string
	Columns []string
}

type LocatorStrategy struct {
	Table    string   `json:"table"`
	Strategy string   `json:"strategy"`
	Index    string   `json:"index,omitempty"`
	Columns  []string `json:"columns"`
}

type PreflightResult struct {
	OK                  bool               `json:"ok"`
	LogBin              bool               `json:"log_bin"`
	BinlogFormat        string             `json:"binlog_format"`
	BinlogRowImage      string             `json:"binlog_row_image"`
	GTIDMode            string             `json:"gtid_mode"`
	RetentionSecs       *int64             `json:"binlog_retention_seconds"`
	CurrentPosition     Position           `json:"current_position"`
	Tables              []TableInfo        `json:"tables"`
	NoPrimaryKey        []string           `json:"no_primary_key_tables"`
	MissingTargetTables []TargetTableIssue `json:"missing_target_tables"`
	ExcludedTables      []TargetTableIssue `json:"excluded_tables"`
	Errors              []string           `json:"errors"`
	Warnings            []string           `json:"warnings"`
}

type TargetTableIssue struct {
	SourceTable  string `json:"source_table"`
	TargetSchema string `json:"target_schema"`
	TargetTable  string `json:"target_table"`
}

type Config struct {
	JobID                  string
	SourceDSN              string
	SourceHost             string
	SourcePort             uint16
	SourceUser             string
	SourcePassword         string
	SourceDatabase         string
	TargetDSN              string
	TargetDBType           string
	TargetSchema           string
	Mode                   string
	Filter                 string
	TableNames             []string
	RequestedExclusions    []string
	ScopeExclusions        []BootstrapIssue
	ScopeManifestHash      string
	LowerCaseNames         bool
	ServerID               uint32
	Start                  Position
	KeylessChangePolicy    string
	LocatorStrategyVersion int
	LocatorStrategies      []LocatorStrategy
}

type BootstrapIssue struct {
	Table string `json:"table"`
	Stage string `json:"stage"`
	Error string `json:"error"`
	DDL   string `json:"ddl,omitempty"`
}

// BootstrapFailedObject preserves a full-snapshot failure independently from
// the table exclusion manifest. DDL may be empty for data and validation
// failures that still need to be visible in the diagnostic export.
type BootstrapFailedObject struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Error    string `json:"error"`
	DDL      string `json:"ddl,omitempty"`
	Stage    string `json:"stage"`
}

type BootstrapRecord struct {
	State                  string                  `json:"state"`
	Position               Position                `json:"position"`
	EffectiveTables        []string                `json:"effective_tables"`
	ExcludedTables         []BootstrapIssue        `json:"excluded_tables"`
	ManifestHash           string                  `json:"manifest_hash"`
	FailedObjects          []BootstrapFailedObject `json:"failed_objects"`
	FailureReportVersion   int                     `json:"failure_report_version"`
	LocatorStrategyVersion int                     `json:"locator_strategy_version"`
	LocatorStrategies      []LocatorStrategy       `json:"locator_strategies"`
}

type BootstrapReview struct {
	BootstrapRecord
	RequestedCount int      `json:"requested_count"`
	Warnings       []string `json:"warnings"`
}

type Stats struct {
	Inserts, Updates, Deletes, Skipped, Warnings int64
	Position                                     Position
	SourceHead                                   Position
	CaughtUp                                     bool
	LagSeconds                                   int64
	LastEventAt                                  time.Time
}

type CountValidation struct {
	Table  string `json:"table"`
	Source int64  `json:"source"`
	Target int64  `json:"target"`
	Match  bool   `json:"match"`
	Error  string `json:"error,omitempty"`
}

type Hooks struct {
	Status      func(status, phase, summary string)
	Stats       func(Stats)
	DDL         func(sql string, pos Position)
	RowConflict func(RowConflict)
}

type RowConflict struct {
	Table      string   `json:"table"`
	Action     string   `json:"action"`
	Position   Position `json:"position"`
	Error      string   `json:"error"`
	BeforeHash string   `json:"before_hash"`
}

type Change struct {
	Action string
	Table  *TableInfo
	Before []any
	After  []any
}
