package db_test

import (
	"errors"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/uid"
)

// TestConstraintPredicates provokes a real violation of each class against the real
// schema, on whichever engine dbtest is configured for, and asserts the predicate
// recognises it.
//
// Provoked rather than constructed: a hand-built error would only prove the
// predicate agrees with whatever the test author believed the driver returns, and
// the whole reason these helpers exist is that that belief was wrong on PostgreSQL.
// Run it twice — once bare, once with CALNODE_TEST_POSTGRES_DSN — and both engines
// are covered.
func TestConstraintPredicates(t *testing.T) {
	h := dbtest.Open(t)

	// A user and an event type to hang the booking constraints off.
	userID := uid.New()
	if _, err := h.Exec(
		`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, ?, 'Host', 'UTC')`,
		userID, userID+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	etID := uid.New()
	if _, err := h.Exec(
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes)
		 VALUES (?, ?, ?, 'Call', 30)`, etID, userID, etID); err != nil {
		t.Fatalf("seed event type: %v", err)
	}

	t.Run("unique", func(t *testing.T) {
		// users.email is UNIQUE in both migration sets.
		_, err := h.Exec(
			`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, ?, 'Clash', 'UTC')`,
			uid.New(), userID+"@example.com")
		assertOnly(t, err, "unique", db.IsUniqueViolation)
	})

	t.Run("check", func(t *testing.T) {
		// bookings.status has CHECK (status IN ('confirmed','cancelled')).
		_, err := h.Exec(`
			INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at, updated_at)
			VALUES (?, ?, ?, '2026-06-15T09:00:00Z', '2026-06-15T09:30:00Z', 'not-a-status', ?, ?)`,
			uid.New(), etID, userID, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z")
		assertOnly(t, err, "check", db.IsCheckViolation)
	})

	t.Run("foreign key", func(t *testing.T) {
		// event_type_id references event_types(id). SQLite needs foreign_keys=ON,
		// which OpenDB sets.
		_, err := h.Exec(`
			INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at, updated_at)
			VALUES (?, 'no-such-event-type', ?, '2026-06-15T10:00:00Z', '2026-06-15T10:30:00Z', 'confirmed', ?, ?)`,
			uid.New(), userID, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z")
		assertOnly(t, err, "foreign key", db.IsForeignKeyViolation)
	})

	t.Run("unrelated error", func(t *testing.T) {
		// A predicate that answered true for everything would satisfy every caller
		// above and be badly wrong, so the negative cases carry as much weight.
		_, err := h.Exec(`SELECT * FROM a_table_that_does_not_exist`)
		if err == nil {
			t.Fatal("expected an error from a missing table")
		}
		assertNone(t, err, "missing table")
		assertNone(t, errors.New("some unrelated failure"), "plain error")
		assertNone(t, nil, "nil")
	})
}

// assertOnly checks that want recognises err and the other two predicates do not:
// the classes have to be distinguishable, or a CHECK violation becomes a 409.
func assertOnly(t *testing.T, err error, class string, want func(error) bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a constraint violation, got nil", class)
	}
	if !want(err) {
		t.Errorf("%s: predicate did not recognise %v", class, err)
	}
	matches := 0
	for _, p := range []func(error) bool{db.IsUniqueViolation, db.IsCheckViolation, db.IsForeignKeyViolation} {
		if p(err) {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("%s: %d of 3 predicates matched %v; want exactly 1", class, matches, err)
	}
}

func assertNone(t *testing.T, err error, what string) {
	t.Helper()
	for name, p := range map[string]func(error) bool{
		"IsUniqueViolation":     db.IsUniqueViolation,
		"IsCheckViolation":      db.IsCheckViolation,
		"IsForeignKeyViolation": db.IsForeignKeyViolation,
	} {
		if p(err) {
			t.Errorf("%s returned true for %s (%v)", name, what, err)
		}
	}
}
