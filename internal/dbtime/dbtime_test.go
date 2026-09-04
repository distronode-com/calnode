package dbtime_test

import (
	"regexp"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtime"
)

// The whole point of this package is that a Go-computed timestamp is
// indistinguishable from the one SQLite used to write. Assert that against SQLite
// itself rather than against a hand-read of its docs: if the shapes ever diverge,
// existing rows and new rows stop comparing lexicographically and the recordings
// consent window silently returns the wrong set.
func TestLayoutsMatchSQLite(t *testing.T) {
	handle, err := db.OpenDB("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.OpenDB: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	cases := []struct {
		name string
		expr string
		got  string
	}{
		{"datetime('now')", `SELECT datetime('now')`, dbtime.Now()},
		{"strftime millis", `SELECT strftime('%Y-%m-%dT%H:%M:%fZ','now')`, dbtime.NowMilli()},
	}

	for _, c := range cases {
		var want string
		if err := handle.QueryRow(c.expr).Scan(&want); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// Compare shape, not value: the two clocks are read microseconds apart, so
		// the digits legitimately differ. A digit-for-digit mask is what catches a
		// layout mistake (missing "Z", " " where "T" belongs, 6 fractional digits).
		if mask(want) != mask(c.got) {
			t.Errorf("%s: sqlite wrote %q (shape %q), dbtime produced %q (shape %q)",
				c.name, want, mask(want), c.got, mask(c.got))
		}
	}
}

var digits = regexp.MustCompile(`[0-9]`)

func mask(s string) string { return digits.ReplaceAllString(s, "N") }
