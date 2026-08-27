package dialect

import "dbgold/datamigrate/source"

// IndexName and SequenceName are shared by SQL generation and migration reports.
// Inputs have already been case-normalized by the migrator. Never feed the
// returned index name back into IndexInfo: doing so would add a prefix twice.
func IndexName(d Dialect, idx source.IndexInfo) string {
	if n, ok := d.(interface{ IndexName(source.IndexInfo) string }); ok {
		return n.IndexName(idx)
	}
	return idx.IndexName
}

func SequenceName(d Dialect, seq source.SequenceInfo) string {
	if n, ok := d.(interface {
		SequenceName(source.SequenceInfo) string
	}); ok {
		return n.SequenceName(seq)
	}
	return "seq_" + seq.TableName + "_" + seq.ColumnName
}
