package handler

import "testing"

func TestPGCompatibleDriverName(t *testing.T) {
	tests := map[string]string{
		"postgres": "postgres",
		"highgo":   "highgo",
		"gaussdb":  "opengauss",
		"vastbase": "postgres",
		"gbase":    "postgres",
		"kingbase": "postgres",
		"seabox":   "postgres",
	}
	for dbType, want := range tests {
		t.Run(dbType, func(t *testing.T) {
			if got := pgCompatibleDriverName(dbType); got != want {
				t.Fatalf("pgCompatibleDriverName(%q) = %q, want %q", dbType, got, want)
			}
		})
	}
}
