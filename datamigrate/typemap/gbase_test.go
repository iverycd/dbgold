package typemap

import "testing"

func TestGBasePGTypeMappingsRegistered(t *testing.T) {
	for _, srcType := range []string{"mysql", "oracle", "sqlserver", "dameng"} {
		if _, ok := Get(srcType, "gbase"); !ok {
			t.Errorf("missing %s -> gbase type mapping", srcType)
		}
	}
}
