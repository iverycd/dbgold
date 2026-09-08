package highgo

import (
	"dbgold/driver/pgroutines"
	"dbgold/schema"
)

func (d *Driver) ExtractRoutines(namespace string) ([]schema.Routine, error) {
	return pgroutines.Extract(d.db, namespace, false)
}
func (d *Driver) ExtractTriggers(namespace string) ([]schema.Routine, error) {
	return pgroutines.Extract(d.db, namespace, true)
}
