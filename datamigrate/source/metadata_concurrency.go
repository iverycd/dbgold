package source

import "database/sql"

// MetadataReadConcurrency is an optional capability for independent, read-only
// GetTableDDLInfo calls. Reader implementations without it remain serial.
// Snapshot lifecycles must remain unchanged while metadata is being read.
func (r *MySQLReader) MetadataReadConcurrency() int {
	if r.snapshotTx != nil {
		return 1
	}
	return metadataReadConcurrency(r.db)
}

func (r *SQLServerReader) MetadataReadConcurrency() int { return metadataReadConcurrency(r.db) }
func (r *OracleReader) MetadataReadConcurrency() int    { return metadataReadConcurrency(r.db) }
func (r *DaMengReader) MetadataReadConcurrency() int    { return metadataReadConcurrency(r.db) }
func (r *PostgresReader) MetadataReadConcurrency() int  { return metadataReadConcurrency(r.db) }

func metadataReadConcurrency(db *sql.DB) int {
	if db == nil {
		return 1
	}
	if limit := db.Stats().MaxOpenConnections; limit > 0 && limit < 4 {
		return limit
	}
	return 4
}
