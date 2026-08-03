package target

// GBaseWriter writes to GBase in PostgreSQL compatibility mode.
// GBase uses the PostgreSQL wire protocol, value conversion, and dialect.
type GBaseWriter struct {
	*PostgresWriter
}

func (w *GBaseWriter) DBType() string { return "gbase" }

// NewGBase creates a Writer for a PostgreSQL-compatible GBase target.
func NewGBase(dsn, schema string, pool ConnPoolConfig) (*GBaseWriter, error) {
	writer, err := NewPostgresCompatible(dsn, schema, "gbase", pool)
	if err != nil {
		return nil, err
	}
	return &GBaseWriter{PostgresWriter: writer}, nil
}
