package damengnative

// The transactional NULL row used to advance an empty base's DML epoch must
// not evaluate user index expressions (including indexes on virtual columns).
// These indexes are created after history recovery, before final verification.
// Constraint-owned indexes remain part of their original table definition.
func (r *catalogReader) deferMaterializedEmptyIndexes() {
	empty := map[string]bool{}
	for _, object := range r.snapshot.Objects {
		if object.Materialized == nil || object.Materialized.History == nil {
			continue
		}
		for _, view := range object.Materialized.History.Views {
			if view.EmptyTouch != nil {
				empty[view.EmptyTouch.Table] = true
			}
		}
	}
	for i := range r.snapshot.Objects {
		object := &r.snapshot.Objects[i]
		if object.Managed || !empty[object.Table] || object.Kind != "index" && object.Kind != "context_index" {
			continue
		}
		for j := range object.Units {
			if object.Units[j].Phase == "" || object.Units[j].Phase == "after_data" {
				object.Units[j].Phase = "after_history"
			}
		}
	}
}
