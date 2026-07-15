package cdc

import "time"

const CheckpointTableName = "__dbgold_cdc_checkpoint"

type Position struct {
	File string `json:"file"`
	Pos  uint32 `json:"position"`
	GTID string `json:"gtid"`
}

type TableInfo struct {
	Name          string   `json:"name"`
	Engine        string   `json:"engine"`
	Columns       []string `json:"columns"`
	ColumnTypes   []string `json:"column_types"`
	PrimaryKey    []int    `json:"primary_key_indexes"`
	AutoIncrement []int    `json:"auto_increment_indexes"`
}

type PreflightResult struct {
	OK              bool        `json:"ok"`
	LogBin          bool        `json:"log_bin"`
	BinlogFormat    string      `json:"binlog_format"`
	BinlogRowImage  string      `json:"binlog_row_image"`
	GTIDMode        string      `json:"gtid_mode"`
	RetentionSecs   *int64      `json:"binlog_retention_seconds"`
	CurrentPosition Position    `json:"current_position"`
	Tables          []TableInfo `json:"tables"`
	NoPrimaryKey    []string    `json:"no_primary_key_tables"`
	Errors          []string    `json:"errors"`
	Warnings        []string    `json:"warnings"`
}

type Config struct {
	JobID          string
	SourceDSN      string
	SourceHost     string
	SourcePort     uint16
	SourceUser     string
	SourcePassword string
	SourceDatabase string
	TargetDSN      string
	TargetSchema   string
	Mode           string
	Filter         string
	TableNames     []string
	LowerCaseNames bool
	ServerID       uint32
	Start          Position
}

type BootstrapIssue struct {
	Table string `json:"table"`
	Stage string `json:"stage"`
	Error string `json:"error"`
	DDL   string `json:"ddl,omitempty"`
}

type BootstrapRecord struct {
	State           string           `json:"state"`
	Position        Position         `json:"position"`
	EffectiveTables []string         `json:"effective_tables"`
	ExcludedTables  []BootstrapIssue `json:"excluded_tables"`
	ManifestHash    string           `json:"manifest_hash"`
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
	Status func(status, phase, summary string)
	Stats  func(Stats)
	DDL    func(sql string, pos Position)
}

type Change struct {
	Action string
	Table  *TableInfo
	Before []any
	After  []any
}
