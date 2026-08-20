package highgo

import (
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
)

func TestSM3KnownVector(t *testing.T) {
	const want = "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"
	if got := Sm3ToString("abc"); got != want {
		t.Fatalf("Sm3ToString(abc) = %q, want %q", got, want)
	}
}

func TestHighGoAndPostgresDriversRegistered(t *testing.T) {
	drivers := make(map[string]bool)
	for _, name := range sql.Drivers() {
		drivers[name] = true
	}
	for _, name := range []string{"highgo", "postgres"} {
		if !drivers[name] {
			t.Fatalf("database/sql driver %q is not registered; registered=%v", name, sql.Drivers())
		}
	}
}
