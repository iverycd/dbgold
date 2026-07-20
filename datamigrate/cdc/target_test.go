package cdc

import "testing"

func TestTargetDriverName(t *testing.T) {
	tests := map[string]string{
		"":         "postgres",
		"postgres": "postgres",
		"highgo":   "postgres",
		"kingbase": "postgres",
		"seabox":   "postgres",
		"gaussdb":  "opengauss",
	}
	for dbType, expected := range tests {
		t.Run(dbType, func(t *testing.T) {
			if actual := targetDriverName(dbType); actual != expected {
				t.Fatalf("targetDriverName(%q) = %q, want %q", dbType, actual, expected)
			}
		})
	}
}
