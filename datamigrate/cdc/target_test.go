package cdc

import "testing"

func TestTargetDriverName(t *testing.T) {
	tests := map[string]string{
		"":         "postgres",
		"dameng":   "dm",
		"postgres": "postgres",
		"highgo":   "highgo",
		"vastbase": "postgres",
		"gbase":    "postgres",
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
