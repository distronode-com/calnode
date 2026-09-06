package handler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/calnode/calnode/internal/caldav"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// The calendar half of Boundary 5.
//
// Providers are the one integration whose per-tenant state is not a credential in
// server_settings but a ROW in calendar_connections — so the failure mode is not
// "one tenant's API key sends another's email", it is "one tenant's free/busy
// decides another's availability". A provider built at boot holds the handle boot
// gave it, which on a multi-tenant instance is the unbound one; before ForDB
// existed that made the whole integration inert.

// testEncKey is a fixed 64-hex AES-256 key. The provider needs one to construct;
// nothing here encrypts or decrypts, because what is under test is which ROWS a
// provider can reach, not what is in them.
const testEncKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

// newCalHandler builds a handler over a real pair with a CalDAV provider
// registered. CalDAV is the provider used here because it needs no instance-level
// OAuth app — so the test is about the handle, not about credentials.
func newCalHandler(t *testing.T) (*Handler, *db.DB) {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)

	h := New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://app.calnode.test")

	// The platform-level registry: instance credentials, boot's handle. Exactly
	// what server.New installs.
	base := calendar.NewService(app)
	cdav, err := caldav.New(app, testEncKey)
	if err != nil {
		t.Fatalf("caldav.New: %v", err)
	}
	base.Register(cdav)
	h.SetCalendar(base)

	return h, platform
}

// seedCalWorkspace creates a workspace, its owner, and one CalDAV connection row.
func seedCalWorkspace(t *testing.T, h *Handler, platform *db.DB, wsID string) string {
	t.Helper()
	if _, err := platform.Exec(
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
		wsID, wsID, wsID+".example.com"); err != nil {
		t.Fatalf("seed workspace %s: %v", wsID, err)
	}

	userID := wsID + "-user"
	bound := h.db.ForWorkspace(wsID)
	if _, err := bound.Exec(
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		userID, wsID+"@example.com", wsID); err != nil {
		t.Fatalf("seed user for %s: %v", wsID, err)
	}
	// A connection row with a destination, which is what Connected and
	// HasDestination read. The token columns hold ciphertext the test never
	// decrypts; what is under test is which ROWS a provider can see.
	if _, err := bound.Exec(
		`INSERT INTO calendar_connections
		   (id, user_id, provider, access_token_enc, refresh_token_enc, calendar_id, check_conflicts, is_destination, created_at, account_email)
		 VALUES (?, ?, 'caldav', 'x', 'y', ?, 1, 1, '2026-01-01T00:00:00Z', ?)`,
		wsID+"-conn", userID, wsID+"-calendar", wsID+"@example.com"); err != nil {
		t.Fatalf("seed connection for %s: %v", wsID, err)
	}
	return userID
}

// TestCalendar_providersSeeOnlyTheirWorkspace is the positive half: each
// workspace's service reports its OWN user connected, and reports the other
// workspace's user as not connected even though that row exists.
func TestCalendar_providersSeeOnlyTheirWorkspace(t *testing.T) {
	h, platform := newCalHandler(t)
	ctx := context.Background()

	userA := seedCalWorkspace(t, h, platform, "acme")
	userB := seedCalWorkspace(t, h, platform, "globex")

	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})
	b := h.forWorkspace(&Workspace{ID: "globex", Status: "active"})

	calA, calB := a.getCal(), b.getCal()
	if calA == nil || calB == nil {
		t.Fatal("a workspace got no calendar service")
	}
	if calA == calB {
		t.Fatal("both workspaces got the same calendar service — ForDB did not run")
	}

	// Positive control on the data: through the platform handle both rows exist, so
	// a false below is the policy and not an empty table.
	var rows int
	if err := platform.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_connections`).Scan(&rows); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	if rows != 2 {
		t.Fatalf("the fixture left %d connection rows; want 2", rows)
	}

	for _, tc := range []struct {
		name    string
		svc     *calendar.Service
		own     string
		other   string
		otherWS string
	}{
		{name: "acme", svc: calA, own: userA, other: userB, otherWS: "globex"},
		{name: "globex", svc: calB, own: userB, other: userA, otherWS: "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connected, _, err := tc.svc.Connected(ctx, tc.own)
			if err != nil {
				t.Fatalf("Connected(own): %v", err)
			}
			if !connected {
				t.Errorf("%s cannot see its own connection", tc.name)
			}

			leaked, _, err := tc.svc.Connected(ctx, tc.other)
			if err != nil {
				t.Fatalf("Connected(other): %v", err)
			}
			if leaked {
				t.Errorf("%s can see %s's connection", tc.name, tc.otherWS)
			}

			hasDest, err := tc.svc.HasDestination(ctx, tc.own)
			if err != nil {
				t.Fatalf("HasDestination(own): %v", err)
			}
			if !hasDest {
				t.Errorf("%s cannot see its own destination", tc.name)
			}
			otherDest, err := tc.svc.HasDestination(ctx, tc.other)
			if err != nil {
				t.Fatalf("HasDestination(other): %v", err)
			}
			if otherDest {
				t.Errorf("%s can see %s's destination", tc.name, tc.otherWS)
			}
		})
	}
}

// TestCalendar_disconnectCannotReachAnotherWorkspace is the write half. Disconnect
// deletes rows; on the wrong handle it would delete another tenant's connection.
func TestCalendar_disconnectCannotReachAnotherWorkspace(t *testing.T) {
	h, platform := newCalHandler(t)
	ctx := context.Background()

	_ = seedCalWorkspace(t, h, platform, "acme")
	userB := seedCalWorkspace(t, h, platform, "globex")

	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})

	// A asks to disconnect B's user. It must not matter whether this errors or
	// silently affects nothing — what matters is that B's row survives.
	_ = a.getCal().Disconnect(ctx, userB)

	var n int
	if err := platform.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calendar_connections WHERE workspace_id = ?`, "globex").Scan(&n); err != nil {
		t.Fatalf("count B's connections: %v", err)
	}
	if n != 1 {
		t.Errorf("B has %d connections after A tried to disconnect its user; want 1", n)
	}
}

// TestCalendar_keyIsWhatSeparatesThem is the negative control, in the same shape as
// the mailer one: with the cache key stubbed to "", both workspaces share one
// service and whichever built first decides what the other can see.
func TestCalendar_keyIsWhatSeparatesThem(t *testing.T) {
	h, platform := newCalHandler(t)
	ctx := context.Background()

	userA := seedCalWorkspace(t, h, platform, "acme")
	userB := seedCalWorkspace(t, h, platform, "globex")

	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})
	b := h.forWorkspace(&Workspace{ID: "globex", Status: "active"})

	build := func(scoped *Handler) func() *calendar.Service {
		return func() *calendar.Service {
			h.calMu.RLock()
			base := h.calBase
			h.calMu.RUnlock()
			return base.ForDB(scoped.db)
		}
	}

	// Real keys: each sees only its own.
	realA := h.calCache.get(a.cacheKey(), build(a))
	realB := h.calCache.get(b.cacheKey(), build(b))
	if leaked, _, _ := realA.Connected(ctx, userB); leaked {
		t.Fatal("with real keys, A can already see B")
	}
	if leaked, _, _ := realB.Connected(ctx, userA); leaked {
		t.Fatal("with real keys, B can already see A")
	}

	// Key stubbed to "": B is handed the service built for A.
	collapsed := newTenantCache[*calendar.Service]()
	stubbedA := collapsed.get("", build(a))
	stubbedB := collapsed.get("", build(b))
	if stubbedA != stubbedB {
		t.Fatal("a shared key should have handed out one service")
	}
	sawA, _, err := stubbedB.Connected(ctx, userA)
	if err != nil {
		t.Fatalf("Connected: %v", err)
	}
	if !sawA {
		t.Fatal("the collapsed service should have seen A's connection — the control proves nothing otherwise")
	}
	sawOwn, _, _ := stubbedB.Connected(ctx, userB)
	t.Logf("key stubbed to \"\": B's service sees A's connection = %v, its own = %v", sawA, sawOwn)
	if sawOwn {
		t.Error("the collapsed service saw both workspaces; expected only the one it was built for")
	}
}

// TestCalendar_singleTenantReusesTheRegistry: no rebinding, no second allocation,
// so an existing deployment runs the same objects it always did.
func TestCalendar_singleTenantReusesTheRegistry(t *testing.T) {
	database := dbtest.Open(t)
	h := New(database, slog.New(slog.DiscardHandler))

	base := calendar.NewService(database)
	cdav, err := caldav.New(database, testEncKey)
	if err != nil {
		t.Fatalf("caldav.New: %v", err)
	}
	base.Register(cdav)
	h.SetCalendar(base)

	if got := h.getCal(); got != base {
		t.Error("single-tenant getCal returned a rebound copy; it should be the registry itself")
	}
	if got := h.calCache.size(); got != 1 {
		t.Errorf("calendar cache holds %d entries in single-tenant mode; want 1", got)
	}
	// SetCalendar(nil) must be visible immediately, not shadowed by the cache.
	h.SetCalendar(nil)
	if h.getCal() != nil {
		t.Error("SetCalendar(nil) left a cached service behind")
	}
}
