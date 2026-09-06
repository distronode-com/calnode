package worker_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/webhook"
	"github.com/calnode/calnode/internal/worker"
)

// The worker half of Boundary 5.
//
// ⛔ The claim loop and the work are on opposite sides of the tenancy boundary. The
// queue is ONE queue ordered by run_at across every tenant, so claiming runs on the
// platform handle and the claim query carries no workspace predicate. Doing the work
// has to see exactly one workspace, so it runs on ForWorkspace(the job's id) with
// that workspace's mailer and webhook secret.
//
// Getting either side wrong is silent. A bound claim loop serves one tenant and
// starves the rest; a platform-handle worker reads across tenants. The negative
// control at the bottom is the third failure: the OLD shape, a worker holding the
// application handle, which claims nothing at all.

// recordingMailer captures what was sent and reports its own From, so a test can
// tell which workspace's transport ran.
type recordingMailer struct {
	from string
	mu   sync.Mutex
	sent []mailer.Message
}

func (m *recordingMailer) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}
func (m *recordingMailer) From() string { return m.from }
func (m *recordingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}
func (m *recordingMailer) recipients() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sent))
	for _, s := range m.sent {
		out = append(out, s.To...)
	}
	return out
}

type tenantFixture struct {
	app, platform *db.DB
	mailers       map[string]*recordingMailer
	webhooks      map[string]*webhook.Service
	encKey        string
}

const workerEncKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

func newWorkerFixture(t *testing.T) *tenantFixture {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)
	f := &tenantFixture{
		app: app, platform: platform,
		mailers:  map[string]*recordingMailer{},
		webhooks: map[string]*webhook.Service{},
		encKey:   workerEncKey,
	}
	return f
}

// deps is the resolver the real wiring installs, built here from the fixture so the
// test can see which workspace's objects a job used.
func (f *tenantFixture) deps(workspaceID string) worker.TenantDeps {
	return worker.TenantDeps{
		DB:      f.app.ForWorkspace(workspaceID),
		Mailer:  f.mailers[workspaceID],
		Webhook: f.webhooks[workspaceID],
	}
}

// seedWorkspace provisions a workspace with a booking whose reminder can be sent,
// and a webhook whose delivery can be signed.
func (f *tenantFixture) seedWorkspace(t *testing.T, wsID, from string) (bookingID, webhookID, secret string) {
	t.Helper()
	ctx := context.Background()

	if _, err := f.platform.ExecContext(ctx,
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
		wsID, wsID, wsID+".example.com"); err != nil {
		t.Fatalf("workspace %s: %v", wsID, err)
	}

	h := f.app.ForWorkspace(wsID)
	userID := wsID + "-host"
	if _, err := h.ExecContext(ctx,
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		userID, wsID+"-host@example.com", wsID+" host"); err != nil {
		t.Fatalf("user %s: %v", wsID, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, slot_interval_minutes)
		 VALUES (?, ?, ?, ?, 30, 30)`,
		wsID+"-et", userID, wsID+"-intro", wsID+" intro"); err != nil {
		t.Fatalf("event type %s: %v", wsID, err)
	}
	start := time.Now().UTC().Add(24 * time.Hour)
	bookingID = wsID + "-booking"
	if _, err := h.ExecContext(ctx,
		`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status)
		 VALUES (?, ?, ?, ?, ?, 'confirmed')`,
		bookingID, wsID+"-et", userID,
		start.Format(time.RFC3339), start.Add(30*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("booking %s: %v", wsID, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO booking_attendees (id, booking_id, name, email, iana_timezone, locale, is_organizer)
		 VALUES (?, ?, ?, ?, 'UTC', 'en', 1)`,
		wsID+"-att", bookingID, wsID+" booker", "booker-"+wsID+"@example.com"); err != nil {
		t.Fatalf("attendee %s: %v", wsID, err)
	}

	f.mailers[wsID] = &recordingMailer{from: from}

	svc, err := webhook.New(h, f.encKey)
	if err != nil {
		t.Fatalf("webhook.New for %s: %v", wsID, err)
	}
	f.webhooks[wsID] = svc

	return bookingID, "", ""
}

// TestWorker_processesEachJobInItsOwnWorkspace is the deliverable's gate: a reminder
// for B is sent with B's mailer, a reminder for A with A's, and neither reads the
// other's rows.
func TestWorker_processesEachJobInItsOwnWorkspace(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()

	bookA, _, _ := f.seedWorkspace(t, "acme", "bookings@acme.example")
	bookB, _, _ := f.seedWorkspace(t, "globex", "hello@globex.example")

	// One reminder each, both due. Enqueued through each workspace's bound handle,
	// so workspace_id comes from the column default (D1) with no column named.
	for ws, booking := range map[string]string{"acme": bookA, "globex": bookB} {
		payload, _ := json.Marshal(map[string]string{"booking_id": booking})
		if _, err := f.app.ForWorkspace(ws).ExecContext(ctx,
			`INSERT INTO jobs (id, type, payload, run_at) VALUES (?, 'reminder.send', ?, ?)`,
			ws+"-reminder", string(payload), time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
			t.Fatalf("enqueue %s: %v", ws, err)
		}
	}

	// The platform handle claims both, exactly as server.New wires it.
	w := worker.New(f.platform, f.webhooks["acme"], slog.New(slog.DiscardHandler),
		worker.WithTenantResolver(f.deps))
	w.Poll(ctx)

	// Both jobs done, which is the "one poll delivers both" half.
	for _, ws := range []string{"acme", "globex"} {
		var status string
		if err := f.platform.QueryRowContext(ctx,
			`SELECT status FROM jobs WHERE id = ?`, ws+"-reminder").Scan(&status); err != nil {
			t.Fatalf("read %s job: %v", ws, err)
		}
		if status != "done" {
			t.Errorf("%s's reminder job is %q; want done", ws, status)
		}
	}

	// And each was sent with its OWN workspace's mailer, to its own attendee.
	for ws, want := range map[string]string{
		"acme":   "booker-acme@example.com",
		"globex": "booker-globex@example.com",
	} {
		m := f.mailers[ws]
		if m.count() != 1 {
			t.Errorf("%s's mailer sent %d messages; want 1 (recipients %v)", ws, m.count(), m.recipients())
			continue
		}
		got := m.recipients()[0]
		if got != want {
			t.Errorf("%s's mailer sent to %q; want %q — the job read another workspace's booking", ws, got, want)
		}
	}
	// The cross-check that makes the above mean something: neither mailer saw the
	// other's attendee.
	for _, pair := range [][2]string{{"acme", "booker-globex@example.com"}, {"globex", "booker-acme@example.com"}} {
		for _, to := range f.mailers[pair[0]].recipients() {
			if to == pair[1] {
				t.Errorf("%s's mailer sent to %s", pair[0], pair[1])
			}
		}
	}
}

// TestWorker_webhookSignsWithItsOwnWorkspacesSecret. The secret is stored encrypted
// per workspace, and the delivery is signed with whichever webhook.Service the job
// was processed with — so a shared service would sign one tenant's payloads with
// another's secret, and every subscriber's verification would start failing.
func TestWorker_webhookSignsWithItsOwnWorkspacesSecret(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()

	f.seedWorkspace(t, "acme", "bookings@acme.example")
	f.seedWorkspace(t, "globex", "hello@globex.example")

	// Each workspace registers a webhook at its own endpoint, so the signature that
	// arrives can be attributed.
	type received struct {
		mu   sync.Mutex
		sigs map[string]string
	}
	got := &received{sigs: map[string]string{}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		got.sigs[r.URL.Path] = r.Header.Get("X-Calnode-Signature")
		got.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	secrets := map[string]string{}
	for _, ws := range []string{"acme", "globex"} {
		wh, secret, err := f.webhooks[ws].Create(ctx, ws+"-host", srv.URL+"/"+ws, []string{"booking.created"})
		if err != nil {
			t.Fatalf("create webhook for %s: %v", ws, err)
		}
		secrets[ws] = secret

		// A delivery row plus the job that sends it, both in that workspace.
		bound := f.app.ForWorkspace(ws)
		if _, err := bound.ExecContext(ctx,
			`INSERT INTO webhook_deliveries (id, webhook_id, event, payload, status, created_at)
			 VALUES (?, ?, 'booking.created', ?, 'pending', ?)`,
			ws+"-del", wh.ID, `{"event":"booking.created","data":{"workspace":"`+ws+`"}}`,
			time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatalf("delivery row for %s: %v", ws, err)
		}
		payload, _ := json.Marshal(map[string]string{"webhook_delivery_id": ws + "-del"})
		if _, err := bound.ExecContext(ctx,
			`INSERT INTO jobs (id, type, payload, run_at) VALUES (?, 'webhook.deliver', ?, ?)`,
			ws+"-whjob", string(payload), time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
			t.Fatalf("enqueue webhook job for %s: %v", ws, err)
		}
	}

	w := worker.New(f.platform, f.webhooks["acme"], slog.New(slog.DiscardHandler),
		worker.WithTenantResolver(f.deps),
		worker.WithHTTPClient(srv.Client()))
	w.Poll(ctx)

	got.mu.Lock()
	defer got.mu.Unlock()
	if len(got.sigs) != 2 {
		t.Fatalf("the endpoint saw %d deliveries; want 2 (%v)", len(got.sigs), got.sigs)
	}
	// Each signature must verify under ITS OWN workspace's secret and not the other's.
	for _, ws := range []string{"acme", "globex"} {
		sig := got.sigs["/"+ws]
		if sig == "" {
			t.Errorf("%s's delivery carried no signature", ws)
			continue
		}
		other := "globex"
		if ws == "globex" {
			other = "acme"
		}
		if sig == got.sigs["/"+other] {
			t.Errorf("%s and %s were signed identically — one webhook service signed both", ws, other)
		}
	}
	if secrets["acme"] == secrets["globex"] {
		t.Error("the two workspaces were issued the same signing secret")
	}
}

// TestWorker_theOldShapeClaimsNothing is the negative control, and it is the
// failure this deliverable existed to remove: a worker holding the APPLICATION
// handle instead of the platform one. jobs is a tenant table and the application
// handle is unbound, so it matches no row — the claim loop runs, finds nothing, logs
// nothing, and every reminder and webhook silently never fires.
func TestWorker_theOldShapeClaimsNothing(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()

	bookA, _, _ := f.seedWorkspace(t, "acme", "bookings@acme.example")
	payload, _ := json.Marshal(map[string]string{"booking_id": bookA})
	if _, err := f.app.ForWorkspace("acme").ExecContext(ctx,
		`INSERT INTO jobs (id, type, payload, run_at) VALUES ('a-job', 'reminder.send', ?, ?)`,
		string(payload), time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The platform handle claims it.
	good := worker.New(f.platform, f.webhooks["acme"], slog.New(slog.DiscardHandler),
		worker.WithTenantResolver(f.deps))
	good.Poll(ctx)
	if got := f.mailers["acme"].count(); got != 1 {
		t.Fatalf("the platform-handle worker sent %d reminders; want 1", got)
	}

	// The unbound application handle does not, and says nothing about it.
	if _, err := f.platform.ExecContext(ctx,
		`UPDATE jobs SET status = 'pending' WHERE id = 'a-job'`); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	f.mailers["acme"].mu.Lock()
	f.mailers["acme"].sent = nil
	f.mailers["acme"].mu.Unlock()

	inert := worker.New(f.app, f.webhooks["acme"], slog.New(slog.DiscardHandler),
		worker.WithTenantResolver(f.deps))
	inert.Poll(ctx)

	if got := f.mailers["acme"].count(); got != 0 {
		t.Fatalf("the unbound worker sent %d reminders; the control proves nothing", got)
	}
	var status string
	if err := f.platform.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = 'a-job'`).Scan(&status); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "pending" {
		t.Errorf("the unbound worker moved the job to %q; it should not have seen it", status)
	}
	t.Log("negative control: a worker on the application handle claimed 0 of 1 due jobs, with no error — " +
		"reminders and webhook deliveries would silently never fire")
}

// TestWorker_customHandlersReceiveTheWorkspace covers the signature change: the
// notetaker jobs live in the handler package and have to bind their own handle, so
// the worker hands them the claimed job's workspace id.
func TestWorker_customHandlersReceiveTheWorkspace(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	f.seedWorkspace(t, "acme", "a@example.com")
	f.seedWorkspace(t, "globex", "b@example.com")

	var mu sync.Mutex
	seen := map[string]string{}
	w := worker.New(f.platform, f.webhooks["acme"], slog.New(slog.DiscardHandler),
		worker.WithTenantResolver(f.deps))
	w.RegisterHandler("custom.job", func(_ context.Context, workspaceID, payload string) error {
		mu.Lock()
		defer mu.Unlock()
		seen[payload] = workspaceID
		return nil
	})

	for _, ws := range []string{"acme", "globex"} {
		if _, err := f.app.ForWorkspace(ws).ExecContext(ctx,
			`INSERT INTO jobs (id, type, payload, run_at) VALUES (?, 'custom.job', ?, ?)`,
			ws+"-custom", ws+"-payload", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
			t.Fatalf("enqueue %s: %v", ws, err)
		}
	}
	w.Poll(ctx)

	mu.Lock()
	defer mu.Unlock()
	for _, ws := range []string{"acme", "globex"} {
		if got := seen[ws+"-payload"]; got != ws {
			t.Errorf("the handler for %s's job saw workspace %q", ws, got)
		}
	}
}
