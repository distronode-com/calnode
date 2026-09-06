package handler_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/mailer"
)

// The region's EMAIL_SMTP_* transport as the default for a provisioned tenancy that has
// no email settings of its own (D7/D12).
//
// ⛔ Without it a platform-created workspace confirms its bookings to NOBODY, and every
// signal an operator has says otherwise: the booking is created, the 201 is returned, the
// webhook fires, Noop reports the send succeeded, and isEmailEnabled reads the
// process-wide live wrapper and answers yes. The platform client provisions without
// defaults.smtp deliberately — the region's SMTP belongs to the operator, not to the
// tenant — so this is the ordinary case, not an edge one.
//
// ⛔ The fallback reads the ENV transport, never h.live, and the second half of this test
// is what holds that line. live is hot-swapped by the email-settings save path, so a
// tenant that saved its own SMTP would otherwise have lent its credentials and From
// address to every tenant that has none.
//
// Postgres-only and loudly skipped elsewhere: provisioning a second workspace needs the
// multi-tenant schema, which is PostgreSQL by construction.

// envRecorder is the region transport, recording what it was asked to send so a test can
// tell which workspace's mail went through it.
type envRecorder struct {
	mu   sync.Mutex
	sent []mailer.Message
}

func (m *envRecorder) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *envRecorder) From() string { return "bookings@region.example" }

// sentTo reports whether anything was addressed to addr.
func (m *envRecorder) sentTo(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.sent {
		for _, to := range msg.To {
			if to == addr {
				return true
			}
		}
	}
	return false
}

// newEnvMailerAPI wires the handler the way boot does when EMAIL_SMTP_* is set: the live
// wrapper installed by SetMailer AND the same transport kept unswappable as the region
// default. Returns the platform create route and the public booking route.
func newEnvMailerAPI(t *testing.T) (create http.HandlerFunc, book http.Handler, env *envRecorder, app, platform *db.DB) {
	t.Helper()
	app, platform = dbtest.RequireTenantPair(t)

	env = &envRecorder{}
	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)
	h.SetMailer(mailer.NewLive(env), "https://cal.example.test")
	h.SetEnvMailer(env)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bookings",
		h.Scoped(handler.HostWorkspace, (*handler.Handler).CreateBooking))

	return h.Platform((*handler.Handler).CreateWorkspace), mux, env, app, platform
}

// bookOn posts a booking to a workspace's public host and returns the recorder.
func bookOn(t *testing.T, book http.Handler, host, slug, email, startAt string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event_type_slug": slug,
		"start_at":        startAt,
		"name":            "Booker",
		"email":           email,
		"timezone":        "UTC",
	})
	if err != nil {
		t.Fatalf("encode booking: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Host = host
	rec := httptest.NewRecorder()
	book.ServeHTTP(rec, req)
	return rec
}

// envMailerStartAt is a fixed instant, not a relative one.
//
// ⚠️ TestMain pins bookingNow to 2026-06-01 for the whole test binary, so
// validateBookingTime's min-notice and max-future checks run against THAT clock while
// computeSlots still uses the wall clock. A "tomorrow" computed from time.Now() is
// months past max_future_days on the pinned clock and is refused with a 409 that reads
// as an availability problem. The bodies below raise max_future_days to 60 so this date
// is comfortably inside the window.
const envMailerStartAt = "2026-06-15T09:00:00Z"

// waitFor polls until cond holds or the budget runs out. The confirmation is dispatched
// from a goroutine with its own context — deliberately, so the side effects survive the
// request returning — so there is nothing to synchronise on but the effect itself.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestTenancy_envMailerIsTheDefaultForATenantWithNoEmailSettings(t *testing.T) {
	create, book, env, _, platform := newEnvMailerAPI(t)

	// acme provisions the way the website's platform client actually calls: no
	// defaults.smtp at all. seedWorkspaceSettings still writes a server_settings row, so
	// this is a row with an empty smtp_host rather than a missing row — which is exactly
	// the shape LoadEmailSettingsFromDB reports as "not configured".
	noSMTP := platformBookableBody("acme", "book.acme.example")
	delete(noSMTP["defaults"].(map[string]any), "smtp")
	noSMTP["defaults"].(map[string]any)["event_type"].(map[string]any)["max_future_days"] = 60
	if rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces",
		noSMTP, platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("provision acme: status = %d — %s", rec.Code, rec.Body.String())
	}

	// globex brings its own transport. 127.0.0.1:1 is a closed port on purpose: the send
	// has to fail, and it has to fail by connection-refused in microseconds rather than by
	// a 30-second dial timeout left running past the end of the test.
	own := platformBookableBody("globex", "book.globex.example")
	own["defaults"].(map[string]any)["event_type"].(map[string]any)["max_future_days"] = 60
	own["defaults"].(map[string]any)["smtp"] = map[string]any{
		"host": "127.0.0.1", "port": "1",
		"from": "bookings@globex.example", "from_name": "Globex",
	}
	if rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces",
		own, platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("provision globex: status = %d — %s", rec.Code, rec.Body.String())
	}

	var settingsHost string
	if err := platform.QueryRow(
		`SELECT smtp_host FROM server_settings WHERE workspace_id = 'acme' AND id = 1`).
		Scan(&settingsHost); err != nil {
		t.Fatalf("read acme settings: %v", err)
	}
	if settingsHost != "" {
		t.Fatalf("acme's smtp_host = %q; the case under test is a tenancy with none", settingsHost)
	}

	// The tenancy with no settings: its confirmation goes out over the region transport.
	if rec := bookOn(t, book, "book.acme.example", "intro", "booker@acme-customer.example", envMailerStartAt); rec.Code != http.StatusCreated {
		t.Fatalf("book on acme: status = %d — %s", rec.Code, rec.Body.String())
	}
	if !waitFor(func() bool { return env.sentTo("booker@acme-customer.example") }) {
		t.Fatalf("nothing was sent to acme's booker over the environment transport. A tenancy " +
			"with no email settings resolved to Noop, which reports success and sends nothing — " +
			"the booking is created and confirmed to nobody")
	}

	// The tenancy with its own settings does NOT borrow it. Its send fails against the
	// closed port and is logged; what must not happen is the region transport carrying
	// mail addressed by a workspace that configured its own.
	if rec := bookOn(t, book, "book.globex.example", "intro", "booker@globex-customer.example", envMailerStartAt); rec.Code != http.StatusCreated {
		t.Fatalf("book on globex: status = %d — %s", rec.Code, rec.Body.String())
	}
	// Long enough that a fallback would have shown up: the acme confirmation above
	// arrived over this same recorder, so the check below is an absence measured against
	// a positive control rather than against an empty test.
	time.Sleep(500 * time.Millisecond)
	if env.sentTo("booker@globex-customer.example") {
		t.Error("globex's confirmation went out over the environment transport even though " +
			"globex configured its own — a tenant's own settings must win, and the region " +
			"default must never carry another workspace's From address")
	}
}
