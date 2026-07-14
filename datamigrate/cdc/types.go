package cdc

import "time"

type Position struct {
	File string `json:"file"`
	Pos  uint32 `json:"position"`
	GTID string `json:"gtid"`
}

type TableInfo struct {
	Name          string   `json:"name"`
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
	LowerCaseNames bool
	ServerID       uint32
	Start          Position
}

type Stats struct {
	Inserts, Updates, Deletes, Skipped, Warnings int64
	Position                                     Position
	LastEventAt                                  time.Time
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
