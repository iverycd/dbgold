package repair

import "dbgold/datamigrate/dialect"

func (o Object) TrustedCrossOracleSequence() bool {
	if o.Sequence == nil || o.Sequence.Native == nil || !o.Sequence.Native.OracleCross || o.DiagnosticOnly || o.Schema == "" || o.TargetName != o.Sequence.Native.Name || o.Kind != "sequence" || dialect.ValidateCrossOracleSequence(*o.Sequence) != nil {
		return false
	}
	for _, owned := range []bool{false, true} {
		d := dialect.NewOracle()
		d.OwnedSequences[o.TargetName] = owned
		ss := d.SequenceStatements(o.Schema, *o.Sequence, "")
		if len(ss) != len(o.Units) || len(ss) == 0 {
			continue
		}
		valid := true
		for i, u := range o.Units {
			valid = valid && u.SQL == u.OriginalSQL && u.SQL == ss[i].SQL
		}
		if valid {
			return true
		}
	}
	return false
}
