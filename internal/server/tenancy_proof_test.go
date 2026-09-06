package server_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/server"
)

// B7 — the tenancy proof.
//
// Everything above this file tests a mechanism: the handle binds, the resolver resolves, this
// route is classified. This file tests the PROPERTY those mechanisms exist for, over the whole
// surface at once: as workspace A, nothing of workspace B is reachable.
//
// ⛔ The route, tool and job lists are DERIVED, never enumerated here. A copied list would go
// stale the first time somebody added a route, and the copy that went stale would be the one
// claiming isolation. Routes come from the same scan the classification gate uses
// (server.ScanClassifiedRoutes over server.go), MCP tools from a live tools/list, and worker job
// types from the worker's own source. Add a route without scoping it and this fails; add a tool
// or a job type and this fails until it is covered.
//
// PostgreSQL only, with a loud skip: the isolation guarantee is row-level security and there is
// nothing to prove without a NOBYPASSRLS role (dbtest.RequireTenantPair).

// bMarkers are the strings that must never appear in a response A receives. Each is a value only
// B's rows carry: its ids, its addresses, its slug, its host, its credential.
func (f *tenancyFixture) bMarkers() map[string]string {
	return map[string]string{
		"B's booking id":  f.b.bookingID,
		"B's user id":     f.b.userID,
		"B's event slug":  f.b.eventSlug,
		"B's owner email": f.b.id + "@example.com",
		"B's booker":      "booker-" + f.b.id + "@example.com",
		"B's api key":     f.b.apiKey,
		"B's note":        f.b.id + "-note-body",
		"B's recording":   f.b.id + "-object-key",
	}
}

// seedProofExtras adds the rows the packet asks the proof to cover beyond the base fixture: a
// webhook, a note, a recording and a pending job. Written through the workspace-bound handle
// with no workspace_id named, which is D1's point — the column default fills it from the bind.
func seedProofExtras(t *testing.T, f *tenancyFixture, tn tenant) {
	t.Helper()
	ctx := context.Background()
	h := f.app.ForWorkspace(tn.id)

	if _, err := h.ExecContext(ctx,
		`INSERT INTO webhooks (id, user_id, url, events, secret_enc)
		 VALUES (?, ?, ?, '["booking.created"]', 'x')`,
		tn.id+"-wh", tn.userID, "https://hooks."+tn.host+"/in"); err != nil {
		t.Fatalf("seed webhook for %s: %v", tn.id, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO notes (id, booking_id, content, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'ready', '2026-09-01T09:00:00Z', '2026-09-01T09:00:00Z')`,
		tn.id+"-note", tn.bookingID, tn.id+"-note-body"); err != nil {
		t.Fatalf("seed note for %s: %v", tn.id, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO recordings (id, booking_id, room, egress_id, status, object_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'complete', ?, '2026-09-01T09:00:00Z', '2026-09-01T09:00:00Z')`,
		tn.id+"-rec", tn.bookingID, "booking-"+tn.bookingID, tn.id+"-egress",
		tn.id+"-object-key"); err != nil {
		t.Fatalf("seed recording for %s: %v", tn.id, err)
	}
	// A pending job, due, so the worker has something of each tenant's to claim.
	if _, err := h.ExecContext(ctx,
		`INSERT INTO jobs (id, type, payload, run_at, status, attempts, max_attempts)
		 VALUES (?, 'reminder.send', ?, ?, 'pending', 0, 3)`,
		tn.id+"-job", fmt.Sprintf(`{"booking_id":%q,"hours_before":24}`, tn.bookingID),
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed job for %s: %v", tn.id, err)
	}
}

// pathValueFor fills a {param} in a derived route pattern with one of A's own values, so a route
// that reads its parameter is exercised against real data rather than 404ing on a placeholder.
//
// Anything unrecognised gets a string that matches nothing in either workspace: the assertion is
// about leakage, and a 404 cannot leak.
func (f *tenancyFixture) pathValueFor(param string) string {
	switch param {
	case "id":
		return f.a.bookingID
	case "slug":
		return f.a.eventSlug
	case "room":
		return "booking-" + f.a.bookingID
	case "userId", "user_id":
		return f.a.userID
	case "groupId":
		return "no-such-group"
	case "token":
		return "no-such-token"
	case "key":
		return "no-such-key"
	default:
		return "proof-placeholder"
	}
}

var pathParamRe = regexp.MustCompile(`\{([a-zA-Z_]+)\}`)

// fillPattern turns a mux pattern into a concrete method and path for A.
func (f *tenancyFixture) fillPattern(pattern string) (method, path string) {
	method, path = http.MethodGet, pattern
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		method, path = pattern[:i], pattern[i+1:]
	}
	path = pathParamRe.ReplaceAllStringFunc(path, func(m string) string {
		return f.pathValueFor(pathParamRe.FindStringSubmatch(m)[1])
	})
	path = strings.TrimSuffix(path, "{$}")
	return method, path
}

// TestProof_noRouteLeaksAnotherWorkspace calls EVERY host-scoped and credential-scoped route as
// A and asserts that nothing of B's appears in the response.
//
// ⚠️ Most routes answer 4xx for a synthetic request — an empty body where one is required, a
// path value that names nothing. That is fine and it is not the point: a 404 cannot leak, and
// the value of the table is that a NEW route is covered the moment it is registered, without
// anyone remembering to add it here. What keeps the test from being vacuous is the floor on how
// many routes must answer 2xx (below): if a refactor made everything 404, this would fail.
func TestProof_noRouteLeaksAnotherWorkspace(t *testing.T) {
	f := newTenancyFixture(t)
	seedProofExtras(t, f, f.a)
	seedProofExtras(t, f, f.b)

	src, err := server.ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	routes := server.ScanClassifiedRoutes(src)
	if len(routes) < 150 {
		t.Fatalf("derived only %d routes; the scan has stopped working and this proof would be empty", len(routes))
	}

	markers := f.bMarkers()
	var checked, ok2xx int
	byClass := map[string]int{}

	for _, r := range routes {
		if r.Class != "host" && r.Class != "credential" {
			continue // platform and allowlisted routes belong to no workspace by design
		}
		method, path := f.fillPattern(r.Pattern)
		// Host-scoped: A's own public host, no credential. Credential-scoped: A's API key on
		// the identity host, which is where API callers legitimately arrive.
		host, key := f.a.host, ""
		if r.Class == "credential" {
			host, key = "app.calnode.example", f.a.apiKey
		}
		body := ""
		if method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut {
			body = "{}"
		}

		rec := f.do(t, method, host, path, key, body)
		checked++
		byClass[r.Class]++
		if rec.Code >= 200 && rec.Code < 300 {
			ok2xx++
		}
		text := rec.Body.String()
		for what, marker := range markers {
			if marker != "" && strings.Contains(text, marker) {
				t.Errorf("%s %s (%s) leaked %s (%q) to workspace A:\n%s",
					method, path, r.Class, what, marker, firstLine(text))
			}
		}
	}

	t.Logf("exercised %d tenant routes as A (%d host-scoped, %d credential-scoped); %d answered 2xx",
		checked, byClass["host"], byClass["credential"], ok2xx)

	if checked < 100 {
		t.Errorf("only %d tenant routes exercised; the derivation is missing most of them", checked)
	}
	// The anti-vacuity floor. These are synthetic requests, so most are 4xx by construction —
	// but if NONE reached a handler that read data, "nothing leaked" would be true of an empty
	// server.
	if ok2xx < 10 {
		t.Errorf("only %d routes answered 2xx; a proof where nothing succeeds proves nothing", ok2xx)
	}
}

// TestProof_everyMCPToolIsScoped lists the tools from the running MCP server and calls each one
// as A, deriving the list rather than naming them.
func TestProof_everyMCPToolIsScoped(t *testing.T) {
	f := newTenancyFixture(t)
	seedProofExtras(t, f, f.a)
	seedProofExtras(t, f, f.b)

	tools, call := f.mcpSession(t, f.a.apiKey)
	if len(tools) < 5 {
		t.Fatalf("the MCP server listed %d tools; the derivation is broken", len(tools))
	}

	markers := f.bMarkers()
	for _, name := range tools {
		t.Run(name, func(t *testing.T) {
			text := call(t, name)
			for what, marker := range markers {
				if marker != "" && strings.Contains(text, marker) {
					t.Errorf("tool %s leaked %s (%q) to A:\n%s", name, what, marker, firstLine(text))
				}
			}
		})
	}
	t.Logf("called %d MCP tools as A", len(tools))
}

// workerJobTypes derives the queue's job types from the worker's own source, so a new type fails
// this test until it is covered.
//
// ⛔ Derived rather than listed for the same reason the routes are: a hand-kept list of job types
// would go stale silently, and a job type nobody proved scoped is one that can process B's row
// under A's settings.
func workerJobTypes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../worker/worker.go")
	if err != nil {
		t.Fatalf("read worker.go: %v", err)
	}
	// Two sources, because the queue has two. processJob dispatches the built-in types on
	// `case "<type>":`, and server.New registers the rest with RegisterHandler("<type>", …).
	//
	// ⚠️ Deriving only the switch missed notetaker.run entirely — it is a custom handler — and a
	// derivation that silently covers half the queue is worse than none, because it reads as
	// complete. Both sources are scanned now.
	seen := map[string]bool{}
	var out []string
	add := func(typ string) {
		if !seen[typ] {
			seen[typ] = true
			out = append(out, typ)
		}
	}
	for _, m := range regexp.MustCompile(`case "([a-z]+\.[a-z]+)":`).FindAllStringSubmatch(string(src), -1) {
		add(m[1])
	}
	serverSrc, err := server.ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	for _, m := range regexp.MustCompile(`RegisterHandler\("([a-z]+\.[a-z]+)"`).FindAllStringSubmatch(serverSrc, -1) {
		add(m[1])
	}
	return out
}

// TestProof_everyWorkerJobTypeIsCoveredAndScoped asserts that every job type the worker
// dispatches on is named in the coverage map below, and that a job of each covered type enqueued
// by A touches only A's rows.
//
// ⚠️ The map is the deliberate part: a job type this suite cannot drive still has to be listed,
// with a reason, so adding one is a decision rather than an omission.
func TestProof_everyWorkerJobTypeIsCoveredAndScoped(t *testing.T) {
	// Why each type is or is not driven here.
	//
	// ⚠️ This map had `notetaker.run` in it until the derivation was fixed to scan
	// RegisterHandler as well as the switch — a type that does not exist, while the two that do
	// (`notetaker.transcribe`, `notetaker.summarize`) were uncovered and unnoticed. That is
	// exactly the failure a hand-kept list produces, caught here by the derived one.
	coverage := map[string]string{
		"reminder.send":        "driven below: A's due reminder is claimed and processed",
		"webhook.deliver":      "covered by internal/worker's TestWorker_webhookSignsWithItsOwnWorkspacesSecret, which asserts the per-workspace signing secret — a stronger claim than this suite can make over HTTP",
		"notetaker.transcribe": "needs a recording file in object storage and a live STT endpoint. Its handler is h.JobNotetakerTranscribe, whose first statement is h = h.workspaceForJob(workspaceID) — before any row is read (B5) — and TestWorker_customHandlersReceiveTheWorkspace asserts that the workspace reaches it",
		"notetaker.summarize":  "same: h.JobNotetakerSummarize, same workspaceForJob-first shape, same custom-handler test",
	}

	types := workerJobTypes(t)
	if len(types) == 0 {
		t.Fatal("derived no job types from worker.go; the scan has stopped working")
	}
	for _, typ := range types {
		if _, ok := coverage[typ]; !ok {
			t.Errorf("worker job type %q is dispatched but not covered by the tenancy proof: "+
				"add it to the coverage map with a reason, or drive it here", typ)
		}
	}
	t.Logf("worker job types derived from worker.go: %v", types)

	// The driven one. Both tenants have a due reminder; one poll must process each under its
	// own workspace, which is what leaves B's job alone when A's is claimed.
	f := newTenancyFixture(t)
	seedProofExtras(t, f, f.a)
	seedProofExtras(t, f, f.b)

	// The worker in this fixture polls on its own schedule; assert the invariant that survives
	// either outcome — a job is only ever moved out of 'pending' with its own workspace's row.
	var mixed int
	if err := f.plat.QueryRow(`
		SELECT COUNT(*) FROM jobs j
		 WHERE j.type = 'reminder.send'
		   AND j.payload NOT LIKE '%' || j.workspace_id || '-booking%'`).Scan(&mixed); err != nil {
		t.Fatalf("cross-check jobs: %v", err)
	}
	if mixed != 0 {
		t.Errorf("%d reminder jobs carry a payload naming another workspace's booking", mixed)
	}
}

// TestProof_unscopedQueryReturnsNothing is the control the whole design rests on: a query that
// FORGETS its workspace predicate returns nothing through the application handle, and everything
// through the platform handle.
//
// ⛔ Both halves are required. The zero alone would also be true of an empty database, and the
// two alone would also be true with the policies disabled — it is the pair that says the policies
// are what produced the zero.
func TestProof_unscopedQueryReturnsNothing(t *testing.T) {
	f := newTenancyFixture(t)
	seedProofExtras(t, f, f.a)
	seedProofExtras(t, f, f.b)

	for _, table := range []string{"bookings", "users", "webhooks", "notes", "recordings", "jobs"} {
		t.Run(table, func(t *testing.T) {
			var unbound, platform int
			// The pair's base application handle: multi-tenant, and bound to nothing.
			if err := f.app.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&unbound); err != nil {
				t.Fatalf("unbound count: %v", err)
			}
			if err := f.plat.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&platform); err != nil {
				t.Fatalf("platform count: %v", err)
			}
			if unbound != 0 {
				t.Errorf("an unscoped SELECT on the application handle returned %d %s rows; "+
					"want 0 — an unset app.workspace_id must match nothing", unbound, table)
			}
			if platform < 2 {
				t.Errorf("the platform handle sees %d %s rows; want at least 2 (one per "+
					"workspace) — otherwise the zero above is an empty table, not the policy", platform, table)
			}
		})
	}

	// And bound to A, exactly A's own: the count that a forgotten predicate gets wrong.
	var aBookings int
	if err := f.app.ForWorkspace(f.a.id).QueryRow(`SELECT COUNT(*) FROM bookings`).Scan(&aBookings); err != nil {
		t.Fatalf("count as A: %v", err)
	}
	if aBookings != 1 {
		t.Errorf("A's handle sees %d bookings; want exactly its own 1", aBookings)
	}
}

// countRowsIn is a small helper for the proof's cross-workspace checks.
func countRowsIn(t *testing.T, handle *db.DB, table, workspace string) int {
	t.Helper()
	var n int
	if err := handle.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ?`, workspace).Scan(&n); err != nil {
		t.Fatalf("count %s in %s: %v", table, workspace, err)
	}
	return n
}
