package handler

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/mailer"
)

// Boundary 4: the hot per-tenant state.
//
// These run against ONE handle with multiTenant set, which is enough for what they
// assert: the cache key, the builder's choice of settings row, and invalidation.
// The reads go through the bound handle, and on a single handle that is the handle
// itself — so what is under test is the CACHE, not the row-level security, which
// internal/db and internal/server already prove.

func newCacheHandler(t *testing.T, multiTenant bool) *Handler {
	t.Helper()
	database := dbtest.Open(t)
	h := New(database, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(multiTenant)
	h.SetBaseURL("https://app.calnode.test")
	return h
}

// newPairHandler is for the cases that have to tell two workspaces apart.
//
// ⛔ A single handle cannot: db.ForWorkspace is the identity function on one that
// did not come from OpenPair, so every "scoped" copy would read the same rows and
// the test would compare a value with itself. It needs the real pair, which needs
// PostgreSQL and a NOBYPASSRLS role — so these skip loudly on SQLite, and the
// pure-cache cases below do not.
func newPairHandler(t *testing.T) (*Handler, *db.DB) {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)
	h := New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://app.calnode.test")
	return h, platform
}

// seedPairSettings writes the workspace and its settings row through the PLATFORM
// handle, naming workspace_id — the platform handle binds ”, so an omitted column
// would land both rows in the default workspace.
func seedPairSettings(t *testing.T, platform *db.DB, wsID, from string) {
	t.Helper()
	if _, err := platform.Exec(
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
		wsID, wsID, wsID+".example.com"); err != nil {
		t.Fatalf("seed workspace %s: %v", wsID, err)
	}
	if _, err := platform.Exec(
		`INSERT INTO server_settings (workspace_id, id, smtp_host, smtp_port, email_from, email_from_name)
		 VALUES (?, 1, 'smtp.example.com', '587', ?, ?)`,
		wsID, from, wsID); err != nil {
		t.Fatalf("seed settings for %s: %v", wsID, err)
	}
}

// TestTenantCache_mailerIsPerWorkspace is the positive half: A's mailer carries
// A's From address and B's carries B's, built lazily from each workspace's own
// server_settings row.
func TestTenantCache_mailerIsPerWorkspace(t *testing.T) {
	h, platform := newPairHandler(t)
	seedPairSettings(t, platform, "acme", "bookings@acme.example")
	seedPairSettings(t, platform, "globex", "hello@globex.example")

	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})
	b := h.forWorkspace(&Workspace{ID: "globex", Status: "active"})

	fromA := senderAddress(t, a.getMailer())
	fromB := senderAddress(t, b.getMailer())

	if fromA != "bookings@acme.example" {
		t.Errorf("A's mailer From = %q; want bookings@acme.example", fromA)
	}
	if fromB != "hello@globex.example" {
		t.Errorf("B's mailer From = %q; want hello@globex.example", fromB)
	}
	if fromA == fromB {
		t.Fatal("both workspaces got the same mailer")
	}

	// Two entries, and the base handler's key is not one of them.
	if got := h.mailerCache.size(); got != 2 {
		t.Errorf("mailer cache holds %d entries; want 2", got)
	}
	// Reading again must not rebuild.
	if again := senderAddress(t, a.getMailer()); again != fromA {
		t.Errorf("second read of A's mailer = %q; want %q", again, fromA)
	}
	if got := h.mailerCache.size(); got != 2 {
		t.Errorf("a repeat read grew the cache to %d entries", got)
	}
}

// TestTenantCache_keyIsWhatSeparatesThem is the negative control, kept in the tree
// rather than run by hand: with the key forced to "" the two workspaces share one
// entry, and whichever built first decides what the other sends.
//
// It reaches into the cache directly with the same builder the accessor uses,
// because cacheKey itself is what is under test.
func TestTenantCache_keyIsWhatSeparatesThem(t *testing.T) {
	h, platform := newPairHandler(t)
	seedPairSettings(t, platform, "acme", "bookings@acme.example")
	seedPairSettings(t, platform, "globex", "hello@globex.example")

	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})
	b := h.forWorkspace(&Workspace{ID: "globex", Status: "active"})

	build := func(scoped *Handler) func() mailer.Mailer {
		return func() mailer.Mailer {
			cfg, err := LoadEmailSettingsFromDB(scoped.db, scoped.encKey)
			if err != nil || cfg == nil {
				return &mailer.Noop{}
			}
			m, _ := BuildMailer(*cfg)
			return m
		}
	}

	// The real keys keep them apart.
	realA := senderAddress(t, h.mailerCache.get(a.cacheKey(), build(a)))
	realB := senderAddress(t, h.mailerCache.get(b.cacheKey(), build(b)))
	if realA == realB {
		t.Fatalf("with real keys both workspaces got %q", realA)
	}

	// The stubbed key collapses them.
	collapsed := newTenantCache[mailer.Mailer]()
	stubbedA := senderAddress(t, collapsed.get("", build(a)))
	stubbedB := senderAddress(t, collapsed.get("", build(b)))
	if stubbedA != stubbedB {
		t.Fatalf("a shared key should have collided: A got %q, B got %q", stubbedA, stubbedB)
	}
	if stubbedB != realA {
		t.Errorf("with the key stubbed to \"\", B sends as %q — A's address is %q", stubbedB, realA)
	}
	t.Logf("key stubbed to \"\": B would send as %q instead of %q", stubbedB, realB)
}

// TestTenantCache_saveInvalidatesOnlyThatWorkspace.
func TestTenantCache_saveInvalidatesOnlyThatWorkspace(t *testing.T) {
	h, platform := newPairHandler(t)
	seedPairSettings(t, platform, "acme", "bookings@acme.example")
	seedPairSettings(t, platform, "globex", "hello@globex.example")

	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})
	b := h.forWorkspace(&Workspace{ID: "globex", Status: "active"})

	_ = a.getMailer()
	beforeB := senderAddress(t, b.getMailer())

	// A saves new settings, the way the settings handler does: write the row, then
	// drop the cached client.
	if _, err := a.db.Exec(
		`UPDATE server_settings SET email_from = ? WHERE id = 1`, "new@acme.example"); err != nil {
		t.Fatalf("update A's settings: %v", err)
	}
	h.mailerCache.invalidate(a.cacheKey())

	if got := senderAddress(t, a.getMailer()); got != "new@acme.example" {
		t.Errorf("after A saved, A's mailer From = %q; want new@acme.example", got)
	}
	if got := senderAddress(t, b.getMailer()); got != beforeB {
		t.Errorf("A's save changed B's mailer to %q; want %q", got, beforeB)
	}
}

// TestTenantCache_singleTenantKeepsOneEntry is the byte-identical promise: with
// MULTI_TENANT unset the key is "" whatever workspace a handle claims, so the map
// cannot grow past one and SetX behaves exactly as the old singleton did.
func TestTenantCache_singleTenantKeepsOneEntry(t *testing.T) {
	h := newCacheHandler(t, false)

	if got := h.cacheKey(); got != "" {
		t.Errorf("single-tenant cacheKey() = %q; want empty", got)
	}
	h.SetLLM(nil)
	h.SetStripe(nil)
	h.SetZoom(nil)
	h.SetLiveKit(nil)

	// forWorkspace is the identity function here, so it cannot introduce a key.
	scoped := h.forWorkspace(&Workspace{ID: "acme"})
	if got := scoped.cacheKey(); got != "" {
		t.Errorf("a scoped handle in single-tenant mode has key %q", got)
	}
	for name, size := range map[string]int{
		"llm": h.llmCache.size(), "stripe": h.stripeCache.size(),
		"zoom": h.zoomCache.size(), "livekit": h.livekitCache.size(),
	} {
		if size != 1 {
			t.Errorf("%s cache holds %d entries in single-tenant mode; want 1", name, size)
		}
	}
	// And a nil client stays nil rather than being rebuilt on every read: nil is a
	// meaningful cached value, not "absent".
	if h.getStripe() != nil {
		t.Error("getStripe rebuilt over an explicitly-set nil")
	}
	if got := h.stripeCache.size(); got != 1 {
		t.Errorf("a read after SetStripe(nil) grew the cache to %d", got)
	}
}

// TestTenantCache_concurrentWorkspaces is the race case: two workspaces hitting the
// same cache at once. Run the package with -race for it to mean anything.
func TestTenantCache_concurrentWorkspaces(t *testing.T) {
	h, platform := newPairHandler(t)
	seedPairSettings(t, platform, "acme", "bookings@acme.example")
	seedPairSettings(t, platform, "globex", "hello@globex.example")

	want := map[string]string{
		"acme":   "bookings@acme.example",
		"globex": "hello@globex.example",
	}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		for ws, from := range want {
			wg.Add(1)
			go func(ws, from string) {
				defer wg.Done()
				scoped := h.forWorkspace(&Workspace{ID: ws, Status: "active"})
				got := senderAddressNoFatal(scoped.getMailer())
				if got != from {
					errs <- ws + " got " + got + ", want " + from
				}
				// Interleave a write, so the cache is read and replaced at once.
				if ws == "globex" {
					h.mailerCache.invalidate(scoped.cacheKey())
				}
			}(ws, from)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestTenantCache_getBuildsOnceUnderContention pins the "first store wins"
// behaviour that lets the builder run outside the lock.
func TestTenantCache_getBuildsOnceUnderContention(t *testing.T) {
	c := newTenantCache[int]()
	var mu sync.Mutex
	builds := 0

	var wg sync.WaitGroup
	results := make([]int, 32)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.get("ws", func() int {
				mu.Lock()
				builds++
				n := builds
				mu.Unlock()
				return n
			})
		}(i)
	}
	wg.Wait()

	// Every caller must see the SAME value, even if more than one builder ran.
	for i, got := range results {
		if got != results[0] {
			t.Fatalf("caller %d got %d, caller 0 got %d — the cache handed out two values", i, got, results[0])
		}
	}
	if c.size() != 1 {
		t.Errorf("cache holds %d entries for one key", c.size())
	}
	t.Logf("%d concurrent gets ran %d builders and returned one value", len(results), builds)
}

// TestTenantCache_llmAndStripeBuildFromTheirOwnRow covers the other two kinds
// with the same shape, so the mailer is not the only one proven.
func TestTenantCache_llmAndStripeBuildFromTheirOwnRow(t *testing.T) {
	h, platform := newPairHandler(t)
	seedPairSettings(t, platform, "acme", "bookings@acme.example")
	seedPairSettings(t, platform, "globex", "hello@globex.example")

	// Only A enables the LLM layer.
	a := h.forWorkspace(&Workspace{ID: "acme", Status: "active"})
	b := h.forWorkspace(&Workspace{ID: "globex", Status: "active"})
	if _, err := a.db.Exec(
		`UPDATE server_settings SET llm_enabled = 1, llm_endpoint = ?, llm_model = ? WHERE id = 1`,
		"https://llm.acme.example/v1", "acme-model"); err != nil {
		t.Fatalf("enable A's LLM: %v", err)
	}

	if a.getLLM() == nil {
		t.Error("A enabled the LLM layer and got no client")
	}
	if b.getLLM() != nil {
		t.Error("B did not enable the LLM layer and got a client")
	}

	// Neither has Stripe credentials, so both cache nil — and cache it, rather than
	// rebuilding on every read.
	if a.getStripe() != nil || b.getStripe() != nil {
		t.Error("a workspace with no Stripe credentials got a client")
	}
	if got := h.stripeCache.size(); got != 2 {
		t.Errorf("stripe cache holds %d entries; want one nil per workspace", got)
	}
}

// senderAddress reads the From address out of whatever transport BuildMailer
// chose, which is the only per-workspace value visible from outside.
func senderAddress(t *testing.T, m mailer.Mailer) string {
	t.Helper()
	got := senderAddressNoFatal(m)
	if got == "" {
		t.Fatalf("mailer %T exposes no From address", m)
	}
	return got
}

func senderAddressNoFatal(m mailer.Mailer) string {
	type fromer interface{ From() string }
	if f, ok := m.(fromer); ok {
		return f.From()
	}
	return ""
}
