package typemap

import "testing"

func TestVastbasePGTypeMappingsRegistered(t *testing.T) {
	for _, srcType := range []string{"mysql", "oracle", "sqlserver", "dameng"} {
		if _, ok := Get(srcType, "vastbase"); !ok {
			t.Errorf("missing %s -> vastbase type mapping", srcType)
		}
	}
}
