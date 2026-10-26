package datamigrate

import "dbgold/datamigrate/repair"

// The caller holds mu, or owns the recorder during preparation. Keep the ordered
// object slice as the source of truth: duplicate IDs still select its first match.
func (r *repairRecorder) rebuildObjectIndex() {
	r.objectIndex = make(map[string]int, len(r.objects))
	for i := range r.objects {
		id := r.objects[i].ID
		if _, exists := r.objectIndex[id]; !exists {
			r.objectIndex[id] = i
		}
	}
	r.indexedObjectCount = len(r.objects)
}

func (r *repairRecorder) objectPosition(id string) int {
	if r.objectIndex == nil || r.indexedObjectCount != len(r.objects) {
		r.rebuildObjectIndex()
	}
	i, exists := r.objectIndex[id]
	if !exists {
		return -1
	}
	// Accommodate callers that rearranged a legacy recorder's slice. Intentional
	// replacements and filtering must rebuild the index before their next lookup.
	if i < 0 || i >= len(r.objects) || r.objects[i].ID != id {
		r.rebuildObjectIndex()
		i, exists = r.objectIndex[id]
		if !exists {
			return -1
		}
	}
	return i
}

func (r *repairRecorder) appendObject(o repair.Object) int {
	if r.objectIndex == nil || r.indexedObjectCount != len(r.objects) {
		r.rebuildObjectIndex()
	}
	i := len(r.objects)
	r.objects = append(r.objects, o)
	if _, exists := r.objectIndex[o.ID]; !exists {
		r.objectIndex[o.ID] = i
	}
	r.indexedObjectCount = len(r.objects)
	return i
}
