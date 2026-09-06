package handler_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/secret"
)

// mustParseKey turns the hex test key into the [32]byte the secret package takes.
func mustParseKey(t *testing.T, hexKey string) [32]byte {
	t.Helper()
	k, err := secret.ParseKey(hexKey)
	if err != nil {
		t.Fatalf("parse test encryption key: %v", err)
	}
	return k
}

// Vendor webhooks under tenancy (B6).
//
// LiveKit and Stripe call us, not a tenant: no tenant Host, no credential of ours. So both
// routes are Platform-classified, and the workspace has to come from the ROW the event names.
// What these tests hold is that an event for B's room or B's checkout session writes into B and
// leaves A alone — the failure being guarded against is the platform handle, which bypasses
// every policy, writing one tenant's event into another tenant's tables.

const (
	vendorHostA = "book.vendor-a.example"
	vendorHostB = "book.vendor-b.example"
)

// newVendorWebhookHandler returns a multi-tenant handler over a real pair, the platform handle,
// and two workspaces each with a booking and per-workspace LiveKit and Stripe credentials.
//
// ⚠️ The credentials differ per workspace on purpose: that is what makes "verified with the
// wrong tenant's secret" a detectable state rather than an invisible one.
func newVendorWebhookHandler(t *testing.T) (*handler.Handler, *db.DB) {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetEncKey(platformTestEncKey)

	for _, ws := range []struct{ id, host, lkSecret string }{
		{"vendor-a", vendorHostA, "secret-for-a"},
		{"vendor-b", vendorHostB, "secret-for-b"},
	} {
		if _, err := platform.Exec(
			`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
			ws.id, ws.id, ws.host); err != nil {
			t.Fatalf("seed workspace %s: %v", ws.id, err)
		}
		// ⚠️ The API secret has to be a real AES-GCM ciphertext under the handler's
		// encryption key, or LoadLiveKitSettingsFromDB returns nil and the handler 200s
		// before it ever verifies a signature — which would make every assertion below
		// about the wrong thing.
		encSecret, encErr := secret.Encrypt(mustParseKey(t, platformTestEncKey), ws.lkSecret)
		if encErr != nil {
			t.Fatalf("encrypt livekit secret: %v", encErr)
		}
		if _, err := platform.Exec(`
			INSERT INTO server_settings (workspace_id, id, smtp_host, smtp_port, email_from, email_from_name,
			                             livekit_url, livekit_api_key, livekit_api_secret_enc)
			VALUES (?, 1, '', '', '', '', ?, ?, ?)`,
			ws.id, "wss://lk."+ws.host, "lk-key-"+ws.id, encSecret); err != nil {
			t.Fatalf("seed settings %s: %v", ws.id, err)
		}
		// A user, an event type and a booking, so a room and a checkout session have
		// something of that workspace's to belong to.
		if _, err := platform.Exec(`
			INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner)
			VALUES (?, ?, ?, 'Owner', 'UTC', 1, 1)`,
			ws.id+"-user", ws.id, "owner@"+ws.id+".example"); err != nil {
			t.Fatalf("seed user %s: %v", ws.id, err)
		}
		if _, err := platform.Exec(`
			INSERT INTO event_types (id, workspace_id, user_id, slug, name, duration_minutes, location_type, routing_mode)
			VALUES (?, ?, ?, 'intro', 'Intro', 30, 'link', 'fixed')`,
			ws.id+"-et", ws.id, ws.id+"-user"); err != nil {
			t.Fatalf("seed event type %s: %v", ws.id, err)
		}
		if _, err := platform.Exec(`
			INSERT INTO bookings (id, workspace_id, event_type_id, host_id, start_at, end_at, status,
			                      created_at, payment_status, stripe_session_id)
			VALUES (?, ?, ?, ?, '2026-10-01T10:00:00Z', '2026-10-01T10:30:00Z', 'confirmed',
			        '2026-09-01T09:00:00Z', 'pending', ?)`,
			ws.id+"-booking", ws.id, ws.id+"-et", ws.id+"-user", "cs_"+ws.id); err != nil {
			t.Fatalf("seed booking %s: %v", ws.id, err)
		}
	}
	return h, platform
}

// seedRecording gives a workspace an active recording row, which is what a LiveKit egress event
// names.
func seedRecording(t *testing.T, platform *db.DB, wsID, room, egressID string) {
	t.Helper()
	if _, err := platform.Exec(`
		INSERT INTO recordings (id, workspace_id, booking_id, room, egress_id, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'active', '2026-09-01T09:00:00Z', '2026-09-01T09:00:00Z')`,
		wsID+"-rec", wsID, wsID+"-booking", room, egressID); err != nil {
		t.Fatalf("seed recording %s: %v", wsID, err)
	}
}

func livekitEgressEndedBody(t *testing.T, egressID, room, key string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event": "egress_ended",
		"egressInfo": map[string]any{
			"egressId": egressID,
			"roomName": room,
			"status":   "EGRESS_COMPLETE",
			"fileResults": []map[string]any{
				{"filename": key, "duration": "60000000000"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return body
}

// ⛔ B's egress event must finalize B's recording and leave A's untouched. Unscoped — which is
// what a Platform route's handle is — the UPDATE is `WHERE egress_id = ?` across every tenant,
// so a colliding or guessed id would reach into any workspace; and the notetaker and the
// recording.completed webhook that follow would run with A's settings on B's data.
func TestVendorWebhook_livekitEventWritesOnlyItsOwnWorkspace(t *testing.T) {
	h, platform := newVendorWebhookHandler(t)
	seedRecording(t, platform, "vendor-a", "booking-vendor-a-booking", "eg-a")
	seedRecording(t, platform, "vendor-b", "booking-vendor-b-booking", "eg-b")

	body := livekitEgressEndedBody(t, "eg-b", "booking-vendor-b-booking", "recordings/b.mp4")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/livekit/webhook", bytes.NewReader(body))
	h.Platform((*handler.Handler).LiveKitWebhook)(rec, req)

	// The signature cannot verify (no real LiveKit key here), so the request is refused — and
	// that is the assertion that matters first: nothing was written on the way to the refusal.
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 403 (signature) or 200 (ignored) — %s", rec.Code, rec.Body.String())
	}
	assertRecordingStatus(t, platform, "vendor-a", "active")
	assertRecordingStatus(t, platform, "vendor-b", "active")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403: the event resolved to a workspace whose LiveKit "+
			"secret cannot verify this body, and an unverifiable vendor call must be refused", rec.Code)
	}
}

// The resolution half, asserted directly: an event naming B's egress resolves to B, one naming
// A's resolves to A, and one naming neither resolves to nothing.
//
// ⛔ This is the part a signature test cannot reach. Verification needs a real LiveKit secret,
// so an end-to-end webhook test can only ever observe the 403; the resolver is where the
// tenancy decision is actually made, and it is exported for the test through a bridge for that
// reason.
func TestVendorWebhook_livekitResolvesTheOwningWorkspace(t *testing.T) {
	h, platform := newVendorWebhookHandler(t)
	seedRecording(t, platform, "vendor-a", "booking-vendor-a-booking", "eg-a")
	seedRecording(t, platform, "vendor-b", "booking-vendor-b-booking", "eg-b")

	for _, c := range []struct {
		name, egressID, room, want string
		found                      bool
	}{
		{"by B's egress id", "eg-b", "", "vendor-b", true},
		{"by A's egress id", "eg-a", "", "vendor-a", true},
		{"by B's room", "", "booking-vendor-b-booking", "vendor-b", true},
		// A room with no recording at all: the booking the room name encodes still names the
		// tenant, which is what a room_finished for an unrecorded meeting needs.
		{"by a booking room with no recording", "", "booking-vendor-a-booking", "vendor-a", true},
		{"unknown egress id", "eg-nobody", "", "", false},
		{"unknown room", "", "booking-nobody", "", false},
		{"nothing at all", "", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := handler.LiveKitEventWorkspaceForTest(h, c.egressID, c.room)
			if ok != c.found {
				t.Fatalf("resolved = %v; want %v", ok, c.found)
			}
			if ok && got != c.want {
				t.Errorf("workspace = %q; want %q", got, c.want)
			}
		})
	}
}

// An event nobody owns is 2xx and writes nothing. A 4xx would make LiveKit retry it for as long
// as it keeps the event, and no retry can make a row exist that never did.
func TestVendorWebhook_livekitUnknownRoomIs2xxWithNoWrite(t *testing.T) {
	h, platform := newVendorWebhookHandler(t)
	seedRecording(t, platform, "vendor-a", "booking-vendor-a-booking", "eg-a")

	body := livekitEgressEndedBody(t, "eg-nobody", "booking-nobody", "recordings/x.mp4")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/livekit/webhook", bytes.NewReader(body))
	h.Platform((*handler.Handler).LiveKitWebhook)(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 — a vendor retry cannot fix an event nobody owns", rec.Code)
	}
	assertRecordingStatus(t, platform, "vendor-a", "active")
}

// ⛔ A body that resolves to a workspace but carries no valid signature is refused, and nothing
// is written on the way there. That is the ordering guarantee: the resolve is one keyed SELECT,
// and every write is behind the verification.
func TestVendorWebhook_livekitBadSignatureWritesNothing(t *testing.T) {
	h, platform := newVendorWebhookHandler(t)
	seedRecording(t, platform, "vendor-b", "booking-vendor-b-booking", "eg-b")

	body := livekitEgressEndedBody(t, "eg-b", "booking-vendor-b-booking", "recordings/b.mp4")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/livekit/webhook", bytes.NewReader(body))
	req.Header.Set("Authorization", "not-a-valid-jwt")
	h.Platform((*handler.Handler).LiveKitWebhook)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 — %s", rec.Code, rec.Body.String())
	}
	assertRecordingStatus(t, platform, "vendor-b", "active")
	var key string
	if err := platform.QueryRow(
		`SELECT object_key FROM recordings WHERE workspace_id = 'vendor-b'`).Scan(&key); err != nil {
		t.Fatalf("read recording: %v", err)
	}
	if key != "" {
		t.Errorf("object_key = %q; a refused webhook must write nothing", key)
	}
}

// The Stripe half of the same property: B's checkout session resolves to B.
func TestVendorWebhook_stripeResolvesTheOwningWorkspace(t *testing.T) {
	h, _ := newVendorWebhookHandler(t)

	for _, c := range []struct {
		name, session, metaBooking, want string
		found                            bool
	}{
		{"by B's session id", "cs_vendor-b", "", "vendor-b", true},
		{"by A's session id", "cs_vendor-a", "", "vendor-a", true},
		// The metadata fallback still takes the workspace from the ROW the booking id finds.
		{"by metadata booking id", "", "vendor-b-booking", "vendor-b", true},
		{"session wins over metadata", "cs_vendor-a", "vendor-b-booking", "vendor-a", true},
		{"unknown session", "cs_nobody", "", "", false},
		{"unknown booking", "", "nobody", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := handler.StripeEventWorkspaceForTest(h, c.session, c.metaBooking)
			if ok != c.found {
				t.Fatalf("resolved = %v; want %v", ok, c.found)
			}
			if ok && got != c.want {
				t.Errorf("workspace = %q; want %q", got, c.want)
			}
		})
	}
}

// A Stripe event for a session nobody owns is 2xx with no write. Stripe retries a 4xx for days.
func TestVendorWebhook_stripeUnknownSessionIs2xxWithNoWrite(t *testing.T) {
	h, platform := newVendorWebhookHandler(t)

	body := []byte(`{"type":"checkout.session.completed","data":{"object":{"id":"cs_nobody",` +
		`"payment_status":"paid","metadata":{"booking_id":"nobody"}}}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", bytes.NewReader(body))
	h.Platform((*handler.Handler).StripeWebhook)(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	for _, ws := range []string{"vendor-a", "vendor-b"} {
		var status string
		if err := platform.QueryRow(
			`SELECT payment_status FROM bookings WHERE workspace_id = ?`, ws).Scan(&status); err != nil {
			t.Fatalf("read %s booking: %v", ws, err)
		}
		if status != "pending" {
			t.Errorf("%s booking payment_status = %q; want pending — an unowned event must "+
				"write nothing anywhere", ws, status)
		}
	}
}

// ⛔ And a Stripe event that DOES resolve, with no valid signature, writes nothing. Both
// bookings stay pending: B's because the signature failed, A's because the event was never its
// own.
func TestVendorWebhook_stripeBadSignatureWritesNothing(t *testing.T) {
	h, platform := newVendorWebhookHandler(t)

	body := fmt.Appendf(nil, `{"type":"checkout.session.completed","data":{"object":{"id":%q,`+
		`"payment_status":"paid","metadata":{"booking_id":%q}}}}`,
		"cs_vendor-b", "vendor-b-booking")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	h.Platform((*handler.Handler).StripeWebhook)(rec, req)

	// No Stripe credentials on these workspaces, so the resolve succeeds and the client is
	// absent: 503. Either way nothing is written, which is the assertion.
	if rec.Code == http.StatusOK {
		t.Errorf("status = 200; an unverifiable Stripe call must not be acknowledged as processed")
	}
	for _, ws := range []string{"vendor-a", "vendor-b"} {
		var status string
		if err := platform.QueryRow(
			`SELECT payment_status FROM bookings WHERE workspace_id = ?`, ws).Scan(&status); err != nil {
			t.Fatalf("read %s booking: %v", ws, err)
		}
		if status != "pending" {
			t.Errorf("%s booking payment_status = %q; want pending", ws, status)
		}
	}
}

func assertRecordingStatus(t *testing.T, platform *db.DB, wsID, want string) {
	t.Helper()
	var status string
	if err := platform.QueryRow(
		`SELECT status FROM recordings WHERE workspace_id = ?`, wsID).Scan(&status); err != nil {
		t.Fatalf("read %s recording: %v", wsID, err)
	}
	if status != want {
		t.Errorf("%s recording status = %q; want %q", wsID, status, want)
	}
}
