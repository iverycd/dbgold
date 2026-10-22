// Package validation compares the original source values with migrated values.
// It never reuses migration cleaners or writes business data to temporary files.
package validation

import "context"

type Kind string

const (
	Text        Kind = "text"
	Binary      Kind = "binary"
	Number      Kind = "number"
	Float       Kind = "float"
	Bool        Kind = "bool"
	Bits        Kind = "bits"
	UUID        Kind = "uuid"
	Date        Kind = "date"
	Time        Kind = "time"
	Timestamp   Kind = "timestamp"
	TimestampTZ Kind = "timestamp_tz"
	JSON        Kind = "json"
	Unsupported Kind = "unsupported"
)

const (
	StatusPassed      = "passed"
	StatusMismatch    = "mismatch"
	StatusError       = "error"
	StatusUnsupported = "unsupported"
	StatusCancelled   = "cancelled"
	StatusIncomplete  = "incomplete"
)

// Column describes the actual database type. Scanners must preserve original
// names and values; temporal/decimal SQL projections must not truncate values.
// FloatBits is 32 or 64 for native binary floating point. Length is BIT width.
type Column struct {
	Name          string `json:"name"`
	DatabaseType  string `json:"database_type"`
	Kind          Kind   `json:"kind"`
	Precision     int64  `json:"precision,omitempty"`
	Scale         int64  `json:"scale,omitempty"`
	Length        int64  `json:"length,omitempty"`
	FloatBits     int    `json:"float_bits,omitempty"`
	TimePrecision int64  `json:"time_precision,omitempty"`
}

type Table struct {
	Columns    []Column
	PrimaryKey []string
}

type Scanner interface {
	DescribeValidationTable(context.Context, string) (Table, error)
	OpenValidationScan(context.Context, string, []Column) (RowStream, error)
}

// Next returns io.EOF after the last row. Returned byte slices only need to
// remain valid until the next call. Scans are a single cursor, never OFFSET.
type RowStream interface {
	Next(context.Context) ([]any, error)
	Close() error
}

// Plan binds source and destination columns by position, using the migration's
// frozen mapping rather than case-insensitive guesses. PrimaryKey uses source
// column names. Empty columns cause Validate to inspect and pair exact names.
type Plan struct {
	SourceTable   string
	TargetTable   string
	SourceColumns []Column
	TargetColumns []Column
	PrimaryKey    []string
}

// Difference keeps fingerprints and bounded diagnostic values in the report.
// Temporary sort files contain fingerprints only.
type Difference struct {
	Type    string                  `json:"type"` // missing | extra | changed | duplicate_key
	KeyHash string                  `json:"key_hash,omitempty"`
	RowHash string                  `json:"row_hash,omitempty"`
	Columns []string                `json:"columns,omitempty"`
	Key     map[string]ValueSummary `json:"key,omitempty"`
	Fields  []FieldDifference       `json:"fields,omitempty"`
}

// ValueSummary bounds report size. Binary values are represented only by their
// byte count and SHA-256; text is limited to 256 Unicode code points.
type ValueSummary struct {
	Text      string `json:"text"`
	Null      bool   `json:"null,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Length    int64  `json:"length,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type FieldDifference struct {
	Column string        `json:"column"`
	Source *ValueSummary `json:"source,omitempty"`
	Target *ValueSummary `json:"target,omitempty"`
}

type TableResult struct {
	SourceTable   string       `json:"source_table"`
	TargetTable   string       `json:"target_table"`
	Status        string       `json:"status"`
	SourceRows    int64        `json:"source_rows"`
	TargetRows    int64        `json:"target_rows"`
	MissingRows   int64        `json:"missing_rows"`
	ExtraRows     int64        `json:"extra_rows"`
	ChangedRows   int64        `json:"changed_rows"`
	DuplicateKeys int64        `json:"duplicate_keys"`
	Samples       []Difference `json:"samples,omitempty"`
	Error         string       `json:"error,omitempty"`
	Note          string       `json:"note,omitempty"`
}

type Report struct {
	Status string        `json:"status"`
	Tables []TableResult `json:"tables"`
}

func (r Report) HasFailures() bool {
	if r.Status != StatusPassed {
		return true
	}
	for _, table := range r.Tables {
		if table.Status != StatusPassed {
			return true
		}
	}
	return false
}

func (r *Report) Finalize() {
	r.Status = StatusPassed
	for _, table := range r.Tables {
		if table.Status != StatusPassed {
			r.Status = table.Status
			return
		}
	}
}
