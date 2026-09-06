package handler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/caldav"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// The reconciler half of Boundary 5.
//
// ⛔ Enumeration is on the platform handle and each pass is on a bound one. Every
// query in a pass reads bookings, booking_hosts and calendar_connections — tenant
// tables — so a pass on the platform handle would reconcile every workspace's
// bookings against whichever connections it resolved, and a pass on the UNBOUND
// application handle would read nothing and heal nothing. Before this it was the
// second of those, which is the failure the negative control below reproduces.

func newReconcileHandler(t *testing.T) (*Handler, *db.DB) {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)

	h := New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://app.calnode.test")

	base := calendar.NewService(app)
	cdav, err := caldav.New(app, testEncKey)
	if err != nil {
		t.Fatalf("caldav.New: %v", err)
	}
	base.Register(cdav)
	h.SetCalendar(base)

	return h, platform
}

// seedOrphan creates a workspace with a CANCELLED booking that still carries a
// calendar event id — the exact divergence reconcileCancellations exists to heal.
// The host has no calendar connection, so CancelEvent has nothing to call and the
// pass clears the id without touching a network.
func seedOrphan(t *testing.T, h *Handler, platform *db.DB, wsID, status string) {
	t.Helper()
	ctx := context.Background()

	if _, err := platform.ExecContext(ctx,
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', ?)`,
		wsID, wsID, wsID+".example.com", status); err != nil {
		t.Fatalf("workspace %s: %v", wsID, err)
	}

	bound := h.db.ForWorkspace(wsID)
	userID := wsID + "-host"
	if _, err := bound.ExecContext(ctx,
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		userID, wsID+"@example.com", wsID); err != nil {
		t.Fatalf("user %s: %v", wsID, err)
	}
	if _, err := bound.ExecContext(ctx,
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, slot_interval_minutes)
		 VALUES (?, ?, ?, ?, 30, 30)`,
		wsID+"-et", userID, wsID+"-intro", wsID+" intro"); err != nil {
		t.Fatalf("event type %s: %v", wsID, err)
	}
	start := time.Now().UTC().Add(24 * time.Hour)
	if _, err := bound.ExecContext(ctx,
		`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status)
		 VALUES (?, ?, ?, ?, ?, 'cancelled')`,
		wsID+"-booking", wsID+"-et", userID,
		start.Format(time.RFC3339), start.Add(30*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("booking %s: %v", wsID, err)
	}
	if _, err := bound.ExecContext(ctx,
		`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary, external_event_id)
		 VALUES (?, ?, ?, 1, ?)`,
		wsID+"-bh", wsID+"-booking", userID, wsID+"-stale-event"); err != nil {
		t.Fatalf("booking host %s: %v", wsID, err)
	}
}

// eventID reads a workspace's stale event id through the platform handle, so the
// assertion is about the row's state rather than about what anyone can see.
func eventID(t *testing.T, platform *db.DB, wsID string) (string, bool) {
	t.Helper()
	var id *string
	if err := platform.QueryRowContext(context.Background(),
		`SELECT external_event_id FROM booking_hosts WHERE workspace_id = ?`, wsID).Scan(&id); err != nil {
		t.Fatalf("read %s's event id: %v", wsID, err)
	}
	if id == nil {
		return "", false
	}
	return *id, true
}

// TestReconciler_enumeratesActiveWorkspacesOnly is the enumeration half, asserted
// exactly: which workspaces a sweep targets, and that each target is bound to its
// own.
func TestReconciler_enumeratesActiveWorkspacesOnly(t *testing.T) {
	h, platform := newReconcileHandler(t)

	seedOrphan(t, h, platform, "acme", "active")
	seedOrphan(t, h, platform, "globex", "suspended")
	seedOrphan(t, h, platform, "initech", "active")

	targets := h.reconcileTargets()

	got := map[string]bool{}
	for _, target := range targets {
		id := target.Workspace().ID
		if got[id] {
			t.Errorf("workspace %s targeted twice", id)
		}
		got[id] = true
		// The binding, not just the label: the handle each pass will query on has
		// to be bound to the same workspace.
		if bound := target.db.Workspace(); bound != id {
			t.Errorf("target %s runs on a handle bound to %q", id, bound)
		}
	}

	if !got["acme"] || !got["initech"] {
		t.Errorf("active workspaces missing from the sweep: %v", got)
	}
	if got["globex"] {
		t.Error("a suspended workspace was included — it answers 503 on its own surfaces")
	}
	// The default workspace exists in every migrated database and is active, so it
	// is expected; what must not appear is a suspended one.
	t.Logf("sweep targets: %v", got)
}

// TestReconciler_healsItsOwnWorkspaceAndSkipsSuspended is the effect half.
func TestReconciler_healsItsOwnWorkspaceAndSkipsSuspended(t *testing.T) {
	h, platform := newReconcileHandler(t)

	seedOrphan(t, h, platform, "acme", "active")
	seedOrphan(t, h, platform, "globex", "suspended")

	// Positive control on the fixture: both rows start stale.
	for _, ws := range []string{"acme", "globex"} {
		if id, ok := eventID(t, platform, ws); !ok || id == "" {
			t.Fatalf("%s's fixture did not leave a stale event id (got %q, present=%v)", ws, id, ok)
		}
	}

	h.reconcileCalendar()

	if _, present := eventID(t, platform, "acme"); present {
		t.Error("the active workspace's stale event id was not cleared")
	}
	id, present := eventID(t, platform, "globex")
	if !present {
		t.Error("the suspended workspace's row was reconciled; it should have been skipped")
	} else if id != "globex-stale-event" {
		t.Errorf("the suspended workspace's event id changed to %q", id)
	}
}

// TestReconciler_theOldShapeHealsNothing is the negative control: one pass on the
// UNBOUND application handle, which is what the reconciler was before this. Every
// query matches no row, so it heals nothing and reports nothing.
func TestReconciler_theOldShapeHealsNothing(t *testing.T) {
	h, platform := newReconcileHandler(t)

	seedOrphan(t, h, platform, "acme", "active")

	// The old shape: one pass, on the handler as boot built it, unbound.
	h.reconcileCalendarPass()

	id, present := eventID(t, platform, "acme")
	if !present {
		t.Fatal("the unbound pass cleared the row; the control proves nothing")
	}
	if id != "acme-stale-event" {
		t.Errorf("the unbound pass changed the event id to %q", id)
	}
	t.Log("negative control: a reconciler pass on the unbound application handle healed 0 of 1 " +
		"diverged bookings, with no error — every stale calendar event would stay stale forever")

	// And the fixed shape does heal it, so the difference is the binding and not the
	// fixture.
	h.reconcileCalendar()
	if _, stillThere := eventID(t, platform, "acme"); stillThere {
		t.Error("the per-workspace sweep did not heal what the unbound pass missed")
	}
}

// TestReconciler_singleTenantIsOneUnenumeratedPass: with MULTI_TENANT unset there is
// no query per sweep and the target is the handler itself, so an existing deployment
// does the same work it always did.
func TestReconciler_singleTenantIsOneUnenumeratedPass(t *testing.T) {
	database := dbtest.Open(t)
	h := New(database, slog.New(slog.DiscardHandler))

	targets := h.reconcileTargets()
	if len(targets) != 1 {
		t.Fatalf("single-tenant sweep has %d targets; want 1", len(targets))
	}
	if targets[0] != h {
		t.Error("the single-tenant target is not the handler itself")
	}

	ids, err := h.activeWorkspaceIDs(context.Background())
	if err != nil {
		t.Fatalf("activeWorkspaceIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != db.DefaultWorkspaceID {
		t.Errorf("activeWorkspaceIDs = %v; want [%s]", ids, db.DefaultWorkspaceID)
	}
}
