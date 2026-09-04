package booking

import (
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// TestHostLockKey pins the derivation. The key must not move between processes or
// releases: two Calnode instances sharing one Postgres have to derive the same key
// for the same host, or they take different locks and serialise against nothing.
//
// The expected values were computed independently of this code (SHA-256 of
// "calnode:booking:host:" + id, first eight bytes big-endian as a signed integer),
// so this fails if the implementation drifts rather than agreeing with itself.
func TestHostLockKey(t *testing.T) {
	cases := map[string]int64{
		"host-42": 7118598067704648523,
		"host-43": 2949734679630933584,
	}
	for id, want := range cases {
		if got := hostLockKey(id); got != want {
			t.Errorf("hostLockKey(%q) = %d; want %d — the derivation changed, which desynchronises running instances",
				id, got, want)
		}
	}
}

// TestLockHosts_sqliteIsNoOp records the other half of the design: SQLite already
// has the guarantee, so the lock must not be attempted there. pg_advisory_xact_lock
// does not exist in SQLite, so if this ever started issuing the statement the error
// would be immediate.
func TestLockHosts_sqliteIsNoOp(t *testing.T) {
	h, err := db.OpenDB("sqlite://:memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()

	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := lockHosts(t.Context(), tx, "a", "b", "a", ""); err != nil {
		t.Errorf("lockHosts on SQLite returned %v; want nil (it must be a no-op there)", err)
	}
}
