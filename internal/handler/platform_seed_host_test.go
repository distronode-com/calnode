package handler_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The seeded event type's HOST ROW (D12), which is what makes a provisioned tenancy
// bookable rather than merely visible.
//
// ⛔ The failure this pins is silent on every surface an operator looks at. The booking
// page renders, /public lists the owner, GET /v1/platform/workspaces/{id} reports a
// healthy workspace — and GET /v1/event-types/{slug}/slots answers 200 with
// {"hosts":{},"slots":[]} forever, because resolveEventTypeHosts reads event_type_hosts
// and NOTHING ELSE. Two tenancies were provisioned that way in production and had the row
// inserted by hand. So the assertion is in two halves: the row itself, and a slots call
// that actually has to produce a slot. The row alone would pass against a row written into
// the wrong workspace; the slots call alone would not say why it failed.
//
// ⛔ Postgres-only, and loudly. Multi-tenant IS PostgreSQL row-level security —
// db.OpenPair refuses a non-postgres DSN, and SQLite's migration 00060 deliberately keeps
// the single-tenant uniques (server_settings' id = 1 among them), so a second workspace
// cannot even be provisioned there. Asserting this on SQLite would be asserting a
// configuration that does not exist.

// newBookablePlatformAPI returns the create route, the public slots route as server.go
// registers it, and the pair the assertions read through: platform to see across the
// tenant boundary, app to see it as the tenancy's own requests do.
func newBookablePlatformAPI(t *testing.T) (create http.HandlerFunc, slots http.Handler, app, platform *db.DB) {
	t.Helper()
	app, platform = dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)

	// The real registration minus CORS and the rate limiter, so the slug arrives as a mux
	// path value and the workspace is resolved from the Host — the two things a direct
	// handler call would quietly get wrong.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/event-types/{slug}/slots",
		h.Scoped(handler.HostWorkspace, (*handler.Handler).GetSlots))

	return h.Platform((*handler.Handler).CreateWorkspace), mux, app, platform
}

// platformBookableBody is the settled create body with an event type that yields slots
// whatever the wall clock says: every day of the week, all day, no notice period, and a
// horizon wider than the range the test asks for. The owner is in UTC because the rules
// are local HH:MM interpreted in the OWNER's zone, and this is a test about a host row
// rather than about timezone arithmetic.
func platformBookableBody(id, host string) map[string]any {
	body := platformCreateBody(id, host)
	body["owner_timezone"] = "UTC"

	availability := make([]map[string]any, 0, 7)
	for dow := 0; dow < 7; dow++ {
		availability = append(availability, map[string]any{
			"day_of_week": dow, "start_time": "00:00", "end_time": "23:59",
		})
	}
	et := body["defaults"].(map[string]any)["event_type"].(map[string]any)
	et["min_notice_minutes"] = 0
	et["max_future_days"] = 14
	et["availability"] = availability
	return body
}

func TestPlatform_seedsTheOwnerAsTheEventTypesRequiredHost(t *testing.T) {
	create, slots, app, platform := newBookablePlatformAPI(t)

	rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces",
		platformBookableBody("acme", "book.acme.example"), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d; want 201 — %s", rec.Code, rec.Body.String())
	}

	var ownerID string
	if err := platform.QueryRow(
		`SELECT id FROM users WHERE workspace_id = 'acme' AND is_owner = 1`).Scan(&ownerID); err != nil {
		t.Fatalf("owner id: %v", err)
	}

	// One host row, the owner's, required, priority 0 — the same shape POST
	// /v1/event-types writes, because a provisioned event type and a hand-created one
	// have to be the same object to everything downstream.
	//
	// ⚠️ Read through the APPLICATION handle bound to acme, not the platform one. The
	// column carries workspace_id, and provisioning runs on the platform handle, which
	// binds '' — so a row written with the column left to its default lands somewhere
	// this tenancy's own requests cannot see, which is the same unbookable page by
	// another route. Counting it here is what makes that visible.
	rows, err := app.ForWorkspace("acme").Query(`
		SELECT eth.user_id, eth.role, eth.priority
		FROM event_type_hosts eth
		JOIN event_types et ON et.id = eth.event_type_id
		WHERE et.slug = 'intro'`)
	if err != nil {
		t.Fatalf("read host rows: %v", err)
	}
	defer rows.Close()
	type hostRow struct {
		userID   string
		role     string
		priority int
	}
	var got []hostRow
	for rows.Next() {
		var hr hostRow
		if err := rows.Scan(&hr.userID, &hr.role, &hr.priority); err != nil {
			t.Fatalf("scan host row: %v", err)
		}
		got = append(got, hr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("host rows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("event_type_hosts rows visible to acme for the seeded event type = %d; want "+
			"exactly 1. resolveEventTypeHosts reads this table and nothing else, so zero rows "+
			"is a tenancy whose booking page renders and whose slots endpoint is permanently empty",
			len(got))
	}
	if got[0].userID != ownerID || got[0].role != "required" || got[0].priority != 0 {
		t.Errorf("host row = %s/%s/%d; want %s/required/0 (the shape POST /v1/event-types seeds)",
			got[0].userID, got[0].role, got[0].priority, ownerID)
	}

	// The positive control: the platform handle sees the same single row, so the 1 above
	// is a row in the right place rather than an empty table read twice.
	var all int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM event_type_hosts WHERE workspace_id = 'acme'`).Scan(&all); err != nil {
		t.Fatalf("count host rows across the boundary: %v", err)
	}
	if all != 1 {
		t.Errorf("the platform handle sees %d host rows for acme; want 1", all)
	}

	// And the row does its job. A count is not proof: the row could name the wrong user,
	// or carry a role the fixed routing mode does not gate on, and the count would still
	// be 1 while the endpoint answered with nothing.
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/event-types/intro/slots?from="+tomorrow.Format("2006-01-02")+
			"&to="+tomorrow.AddDate(0, 0, 6).Format("2006-01-02")+"&tz=UTC", nil)
	req.Host = "book.acme.example"
	slotRec := httptest.NewRecorder()
	slots.ServeHTTP(slotRec, req)
	if slotRec.Code != http.StatusOK {
		t.Fatalf("slots: status = %d; want 200 — %s", slotRec.Code, slotRec.Body.String())
	}

	var out struct {
		Slots []struct {
			Start   string   `json:"start"`
			End     string   `json:"end"`
			HostIDs []string `json:"host_ids"`
		} `json:"slots"`
		Hosts map[string]map[string]string `json:"hosts"`
	}
	if err := json.Unmarshal(slotRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode slots %s: %v", slotRec.Body.String(), err)
	}
	if len(out.Slots) == 0 {
		t.Fatalf("the provisioned event type offered 0 slots over seven all-day windows: %s\n"+
			"an empty slots array with a 200 is exactly how the missing host row presents — the "+
			"booking page renders and nothing can ever be booked", slotRec.Body.String())
	}
	if _, ok := out.Hosts[ownerID]; !ok {
		t.Errorf("hosts = %v; want the owner %s — slots with no host map cannot be attributed",
			out.Hosts, ownerID)
	}
}
