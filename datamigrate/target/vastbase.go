package target

// VastbaseWriter writes to Vastbase in PostgreSQL compatibility mode.
// Vastbase uses the PostgreSQL wire protocol, value conversion, and dialect.
type VastbaseWriter struct {
	*PostgresWriter
}

func (w *VastbaseWriter) DBType() string { return "vastbase" }

// NewVastbase creates a Writer for a PostgreSQL-compatible Vastbase target.
func NewVastbase(dsn, schema string, pool ConnPoolConfig) (*VastbaseWriter, error) {
	writer, err := NewPostgresCompatible(dsn, schema, "vastbase", pool)
	if err != nil {
		return nil, err
	}
	return &VastbaseWriter{PostgresWriter: writer}, nil
}
