package handler_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The platform API (D12): workspace provisioning on the identity host.
//
// ⛔ These need a real OpenPair, not one handle. Provisioning runs on the PLATFORM handle
// and every INSERT names workspace_id, and the failure mode being guarded against — a row
// that does not belong to the tenant it was created for — is invisible unless the
// application handle is a NOBYPASSRLS role that the policies actually constrain.

const platformToken = "platform-token-for-tests"

// platformTestEncKey is a 32-byte AES key in hex. The platform API encrypts the SMTP
// password, the LLM key and the LiveKit secret it is given, so a handler without a key
// would fail provisioning for a reason unrelated to what these tests are about.
const platformTestEncKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// newPlatformAPI returns the four routes as server.New registers them, plus the platform
// handle the assertions read through and the application handle a tenant's own requests
// would use.
func newPlatformAPI(t *testing.T) (routes map[string]http.HandlerFunc, app, platform *db.DB) {
	t.Helper()
	app, platform = dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)

	return map[string]http.HandlerFunc{
		"create": h.Platform((*handler.Handler).CreateWorkspace),
		"get":    h.Platform((*handler.Handler).GetWorkspace),
		"patch":  h.Platform((*handler.Handler).PatchWorkspace),
		"delete": h.Platform((*handler.Handler).DeleteWorkspace),
	}, app, platform
}

// platformCreateBody is the settled contract body, with the fields the website client
// sends. Cases mutate the one field they are about.
func platformCreateBody(id, host string) map[string]any {
	return map[string]any{
		"id":             id,
		"slug":           id,
		"public_host":    host,
		"region":         "us",
		"owner_email":    "owner@" + id + ".example",
		"owner_name":     "Owner " + id,
		"owner_timezone": "America/Toronto",
		"defaults": map[string]any{
			"embed_allowed_origins": []string{"https://" + host},
			"webhook": map[string]any{
				"url":    "https://hooks." + host + "/calnode",
				"fields": []string{"booking_id", "start_at"},
			},
			"event_type": map[string]any{
				"slug":               "intro",
				"name":               "Intro call",
				"duration_minutes":   30,
				"min_notice_minutes": 60,
				"max_future_days":    60,
				"availability": []map[string]any{
					{"day_of_week": 1, "start_time": "09:00", "end_time": "17:00"},
					{"day_of_week": 3, "start_time": "09:00", "end_time": "12:00"},
				},
			},
			"livekit_url":     "wss://lk." + host,
			"livekit_api_key": "lkkey",
			"smtp": map[string]any{
				"host": "smtp." + host, "port": "587",
				"from": "bookings@" + host, "from_name": "Bookings",
			},
		},
	}
}

func doPlatform(t *testing.T, route http.HandlerFunc, method, target string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	// ⚠️ httptest.NewRequest does not populate mux path values — those come from the
	// pattern the ServeMux matched, and these tests call the handler directly. Without
	// this every {id} route reads an empty id and answers 404, which looks exactly like a
	// missing workspace.
	if parts := strings.Split(target, "/"); len(parts) == 5 {
		req.SetPathValue("id", parts[4])
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	route(rec, req)
	return rec
}

// The whole provisioning, asserted row by row through the platform handle — because that
// is the only handle that can see across the tenant boundary, and every row here was
// written by a statement that had to name the tenant itself.
func TestPlatform_createProvisionsTheWholeWorkspace(t *testing.T) {
	routes, _, platform := newPlatformAPI(t)

	rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 — %s", rec.Code, rec.Body.String())
	}

	var out struct {
		APIKey        string `json:"api_key"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.APIKey) < 20 || out.APIKey[:4] != "cno_" {
		t.Errorf("api_key = %q; want a cno_ key", out.APIKey)
	}
	if len(out.WebhookSecret) != 64 {
		t.Errorf("webhook_secret = %q; want 64 hex characters (32 bytes)", out.WebhookSecret)
	}

	// The workspace row.
	var slug, host, region, status string
	if err := platform.QueryRow(
		`SELECT slug, public_host, region, status FROM workspaces WHERE id = 'acme'`).
		Scan(&slug, &host, &region, &status); err != nil {
		t.Fatalf("workspace row: %v", err)
	}
	if slug != "acme" || host != "book.acme.example" || region != "us" || status != "active" {
		t.Errorf("workspace = %s/%s/%s/%s; want acme/book.acme.example/us/active", slug, host, region, status)
	}

	// Every seeded row belongs to acme. Counted with the workspace_id predicate the
	// production statements had to carry, so a row that landed anywhere else is missing
	// here rather than merely misfiled.
	for _, c := range []struct {
		what  string
		query string
	}{
		{"server_settings", `SELECT COUNT(*) FROM server_settings WHERE workspace_id = 'acme' AND id = 1`},
		{"owner user", `SELECT COUNT(*) FROM users WHERE workspace_id = 'acme' AND is_owner = 1`},
		{"api key", `SELECT COUNT(*) FROM api_keys WHERE workspace_id = 'acme'`},
		{"webhook", `SELECT COUNT(*) FROM webhooks WHERE workspace_id = 'acme'`},
		{"event type", `SELECT COUNT(*) FROM event_types WHERE workspace_id = 'acme' AND slug = 'intro'`},
	} {
		var n int
		if err := platform.QueryRow(c.query).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.what, err)
		}
		if n != 1 {
			t.Errorf("%s rows in acme = %d; want 1", c.what, n)
		}
	}
	var rules int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM availability_rules WHERE workspace_id = 'acme'`).Scan(&rules); err != nil {
		t.Fatalf("count availability rules: %v", err)
	}
	if rules != 2 {
		t.Errorf("availability rules = %d; want 2", rules)
	}

	// owner_timezone, not UTC. The availability rules above are local HH:MM interpreted
	// in this timezone, so defaulting it would silently move the workspace's hours.
	var tz string
	if err := platform.QueryRow(
		`SELECT iana_timezone FROM users WHERE workspace_id = 'acme' AND is_owner = 1`).Scan(&tz); err != nil {
		t.Fatalf("owner timezone: %v", err)
	}
	if tz != "America/Toronto" {
		t.Errorf("owner iana_timezone = %q; want America/Toronto (the requested zone, not UTC)", tz)
	}

	// The settings row carries the defaults, including the two columns migration 00062
	// added for values that had no home.
	var origins, smtpHost, lkURL string
	if err := platform.QueryRow(`
		SELECT embed_allowed_origins, smtp_host, livekit_url
		FROM server_settings WHERE workspace_id = 'acme' AND id = 1`).
		Scan(&origins, &smtpHost, &lkURL); err != nil {
		t.Fatalf("settings row: %v", err)
	}
	if origins != "https://book.acme.example" {
		t.Errorf("embed_allowed_origins = %q; want the requested origin", origins)
	}
	if smtpHost != "smtp.book.acme.example" || lkURL != "wss://lk.book.acme.example" {
		t.Errorf("settings = %q / %q; want the requested smtp host and livekit url", smtpHost, lkURL)
	}
}

// The event set the provisioned subscription carries, pinned exactly.
//
// ⛔ It has to be every event this codebase emits. The receiver on the other side handles
// all seven, and a subscription short of that means those events are silently never
// delivered — noticed weeks later as "recordings never appear", with nothing in either
// system to point at. The three media events fire only when recording or the notetaker is
// on, so subscribing to them costs a tenancy nothing.
func TestPlatform_createSubscribesToEveryEmittedEvent(t *testing.T) {
	routes, _, platform := newPlatformAPI(t)

	if rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d — %s", rec.Code, rec.Body.String())
	}

	var eventsJSON string
	if err := platform.QueryRow(
		`SELECT events FROM webhooks WHERE workspace_id = 'acme'`).Scan(&eventsJSON); err != nil {
		t.Fatalf("read the subscription: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(eventsJSON), &got); err != nil {
		t.Fatalf("decode events %q: %v", eventsJSON, err)
	}

	want := []string{
		"booking.created", "booking.cancelled", "booking.rescheduled", "booking.reminder",
		"recording.completed", "transcript.ready", "notes.ready",
	}
	if len(got) != len(want) {
		t.Fatalf("subscribed events = %v; want all %d: %v", got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q; want %q (the order is the stored order, so this pins the "+
				"whole list rather than its membership)", i, got[i], want[i])
		}
	}
}

// Two workspaces provisioned through the same endpoint must not see each other. This is
// the assertion that would fail if any INSERT above had left workspace_id to the column
// default.
func TestPlatform_createdWorkspacesAreIsolated(t *testing.T) {
	routes, app, platform := newPlatformAPI(t)

	for _, ws := range []struct{ id, host string }{
		{"acme", "book.acme.example"},
		{"globex", "book.globex.example"},
	} {
		rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
			platformCreateBody(ws.id, ws.host), platformToken)
		if rec.Code != http.StatusCreated {
			t.Fatalf("provision %s: status = %d — %s", ws.id, rec.Code, rec.Body.String())
		}
	}

	// Through the APPLICATION handle bound to acme, only acme's rows exist — and the
	// count is the one a forgotten predicate would get wrong, so it is the honest probe.
	var users, ets int
	acme := app.ForWorkspace("acme")
	if err := acme.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users as acme: %v", err)
	}
	if err := acme.QueryRow(`SELECT COUNT(*) FROM event_types`).Scan(&ets); err != nil {
		t.Fatalf("count event types as acme: %v", err)
	}
	if users != 1 || ets != 1 {
		t.Errorf("acme sees %d users and %d event types; want 1 and 1 — the other workspace's "+
			"rows are visible", users, ets)
	}

	// And the platform handle sees both, which is what makes the 1s above meaningful
	// rather than an empty database.
	var allUsers int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&allUsers); err != nil {
		t.Fatalf("count all users: %v", err)
	}
	if allUsers != 2 {
		t.Errorf("platform handle sees %d users; want 2", allUsers)
	}
}

func TestPlatform_duplicateIDOrHostIs409(t *testing.T) {
	routes, _, platform := newPlatformAPI(t)

	first := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d — %s", first.Code, first.Body.String())
	}

	sameID := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "other.example"), platformToken)
	if sameID.Code != http.StatusConflict {
		t.Errorf("duplicate id: status = %d; want 409 — %s", sameID.Code, sameID.Body.String())
	}

	body := platformCreateBody("globex", "book.acme.example") // B's id, A's host
	sameHost := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces", body, platformToken)
	if sameHost.Code != http.StatusConflict {
		t.Errorf("duplicate public_host: status = %d; want 409 — %s", sameHost.Code, sameHost.Body.String())
	}

	// ⛔ And the losing create left nothing behind: the whole provisioning is one
	// transaction, so a 409 must not leave a workspace with an owner and a live API key.
	var globex int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = 'globex'`).Scan(&globex); err != nil {
		t.Fatalf("count globex users: %v", err)
	}
	if globex != 0 {
		t.Errorf("a refused create left %d users in globex; the transaction must roll back whole", globex)
	}
}

func TestPlatform_getPatchDelete(t *testing.T) {
	routes, _, platform := newPlatformAPI(t)

	if rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d — %s", rec.Code, rec.Body.String())
	}

	// GET
	rec := doPlatform(t, routes["get"], http.MethodGet, "/v1/platform/workspaces/acme", nil, platformToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d — %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got["id"] != "acme" || got["public_host"] != "book.acme.example" || got["status"] != "active" {
		t.Errorf("get returned %#v; want acme / book.acme.example / active", got)
	}

	missing := doPlatform(t, routes["get"], http.MethodGet, "/v1/platform/workspaces/nope", nil, platformToken)
	if missing.Code != http.StatusNotFound {
		t.Errorf("get unknown: status = %d; want 404", missing.Code)
	}

	// PATCH: suspend and move the host in one call.
	patch := doPlatform(t, routes["patch"], http.MethodPatch, "/v1/platform/workspaces/acme",
		map[string]any{"status": "suspended", "public_host": "vanity.acme.example"}, platformToken)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch: status = %d — %s", patch.Code, patch.Body.String())
	}
	var host, status string
	if err := platform.QueryRow(
		`SELECT public_host, status FROM workspaces WHERE id = 'acme'`).Scan(&host, &status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if host != "vanity.acme.example" || status != "suspended" {
		t.Errorf("after patch: %s / %s; want vanity.acme.example / suspended", host, status)
	}

	bad := doPlatform(t, routes["patch"], http.MethodPatch, "/v1/platform/workspaces/acme",
		map[string]any{"status": "paused"}, platformToken)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("patch to an invalid status: %d; want 400", bad.Code)
	}
	empty := doPlatform(t, routes["patch"], http.MethodPatch, "/v1/platform/workspaces/acme",
		map[string]any{}, platformToken)
	if empty.Code != http.StatusBadRequest {
		t.Errorf("patch with no fields: %d; want 400", empty.Code)
	}

	// A recording with an object key, so DELETE has something to report.
	if _, err := platform.Exec(`
		INSERT INTO recordings (id, workspace_id, room, egress_id, status, object_key)
		VALUES ('rec1', 'acme', 'booking-x', 'eg1', 'complete', 'recordings/acme/rec1.mp4')`); err != nil {
		t.Fatalf("seed recording: %v", err)
	}

	del := doPlatform(t, routes["delete"], http.MethodDelete, "/v1/platform/workspaces/acme", nil, platformToken)
	if del.Code != http.StatusOK {
		t.Fatalf("delete: status = %d — %s", del.Code, del.Body.String())
	}
	var delOut struct {
		Keys []string `json:"recording_object_keys"`
	}
	if err := json.Unmarshal(del.Body.Bytes(), &delOut); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if len(delOut.Keys) != 1 || delOut.Keys[0] != "recordings/acme/rec1.mp4" {
		t.Errorf("recording_object_keys = %#v; want the one seeded key — the objects are the "+
			"caller's to delete and nothing else reports them", delOut.Keys)
	}

	// The cascade: no tenant row survives the workspace.
	for _, table := range []string{"users", "api_keys", "event_types", "availability_rules", "webhooks", "server_settings", "recordings"} {
		var n int
		if err := platform.QueryRow(
			`SELECT COUNT(*) FROM ` + table + ` WHERE workspace_id = 'acme'`).Scan(&n); err != nil {
			t.Fatalf("count %s after delete: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows for the deleted workspace; the ON DELETE CASCADE "+
				"from migration 00060 should have taken them", table, n)
		}
	}

	if again := doPlatform(t, routes["delete"], http.MethodDelete,
		"/v1/platform/workspaces/acme", nil, platformToken); again.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d; want 404", again.Code)
	}
}

// The token, and the two shapes of "off". A wrong token is 401 so an operator can tell a
// typo from a missing feature; an unset token is 404 so a prober cannot tell a
// multi-tenant control plane from an instance that has none.
func TestPlatform_tokenGate(t *testing.T) {
	routes, _, _ := newPlatformAPI(t)

	for name, token := range map[string]string{
		"wrong token": "not-the-token",
		"no token":    "",
	} {
		t.Run(name, func(t *testing.T) {
			rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
				platformCreateBody("acme", "book.acme.example"), token)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d; want 401 — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPlatform_404WithoutATokenConfigured(t *testing.T) {
	database := dbtest.Open(t)
	h := handler.New(database, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true) // multi-tenant, but no token: the API does not exist
	route := h.Platform((*handler.Handler).CreateWorkspace)

	rec := doPlatform(t, route, http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 with CALNODE_PLATFORM_TOKEN unset", rec.Code)
	}
}

// A single-tenant instance has no workspaces to provision, so the API is absent there even
// with a token configured. Same 404, same reasoning as Setup's mirror image.
func TestPlatform_404OnASingleTenantInstance(t *testing.T) {
	database := dbtest.Open(t)
	h := handler.New(database, slog.New(slog.DiscardHandler))
	h.SetPlatformToken(platformToken) // token set, but MULTI_TENANT is not
	route := h.Platform((*handler.Handler).CreateWorkspace)

	rec := doPlatform(t, route, http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on a single-tenant instance", rec.Code)
	}
}

func TestPlatform_createValidation(t *testing.T) {
	cases := map[string]func(map[string]any){
		"id is not a workspace id": func(b map[string]any) { b["id"] = "Not An Id" },
		"id default is reserved":   func(b map[string]any) { b["id"] = "default" },
		"no public_host":           func(b map[string]any) { b["public_host"] = "" },
		"owner_email is not one":   func(b map[string]any) { b["owner_email"] = "nobody" },
		"no owner_timezone":        func(b map[string]any) { b["owner_timezone"] = "" },
		"bad owner_timezone":       func(b map[string]any) { b["owner_timezone"] = "Mars/Olympus" },
		"day_of_week out of range": func(b map[string]any) {
			d := b["defaults"].(map[string]any)
			et := d["event_type"].(map[string]any)
			et["availability"] = []map[string]any{{"day_of_week": 7, "start_time": "09:00", "end_time": "17:00"}}
		},
		"start_time is not HH:MM": func(b map[string]any) {
			d := b["defaults"].(map[string]any)
			et := d["event_type"].(map[string]any)
			et["availability"] = []map[string]any{{"day_of_week": 1, "start_time": "9am", "end_time": "17:00"}}
		},
		"start_time after end_time": func(b map[string]any) {
			d := b["defaults"].(map[string]any)
			et := d["event_type"].(map[string]any)
			et["availability"] = []map[string]any{{"day_of_week": 1, "start_time": "18:00", "end_time": "17:00"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			routes, _, platform := newPlatformAPI(t)
			body := platformCreateBody("acme", "book.acme.example")
			mutate(body)

			rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces", body, platformToken)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 — %s", rec.Code, rec.Body.String())
			}
			var n int
			if err := platform.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id <> 'default'`).Scan(&n); err != nil {
				t.Fatalf("count workspaces: %v", err)
			}
			if n != 0 {
				t.Errorf("a refused create left %d workspaces behind", n)
			}
		})
	}
}
