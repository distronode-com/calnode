package handler_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The SSO hand-off in multi-tenant mode (D11).
//
// ⛔ The endpoint is reached at https://<public_host>/v1/auth/sso, because the cookie has to
// land on the tenant's own domain — that is the entire reason the hand-off exists. So the
// workspace is resolved from the request HOST and the token's `wid` is CHECKED against it: a
// token for workspace A presented on B's host is refused, rather than quietly creating A's
// session on B's domain.
//
// It stays Platform-wrapped rather than Scoped because the handler needs the platform handle
// to write the user and session with an explicit workspace_id — the tenant does not exist as
// far as a bound handle is concerned until those rows do.

const (
	ssoHostA = "book.acme.example"
	ssoHostB = "book.globex.example"
)

// newSSOPairHandler returns a multi-tenant handler over a real OpenPair, the platform handle
// the assertions read through, and the platform-wrapped route as server.New registers it.
func newSSOPairHandler(t *testing.T) (*db.DB, http.HandlerFunc) {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL(ssoBaseURL) // the identity host
	h.SetSSOSecret(ssoSecret)

	seedSSOWorkspace(t, platform, "acme", ssoHostA, "active")
	seedSSOWorkspace(t, platform, "globex", ssoHostB, "active")

	return platform, h.Platform((*handler.Handler).SSOHandoff)
}

func seedSSOWorkspace(t *testing.T, platform *db.DB, id, host, status string) {
	t.Helper()
	if _, err := platform.Exec(
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', ?)`,
		id, id, host, status); err != nil {
		t.Fatalf("seed workspace %s: %v", id, err)
	}
}

// ssoTenantClaims is a valid claim set for one workspace: wid names it and aud is its own
// public host, which is where the token is spent.
func ssoTenantClaims(wid, host string) map[string]any {
	c := ssoClaimSet()
	c["wid"] = wid
	c["aud"] = "https://" + host
	return c
}

// doTenantSSO spends a token AT a host, which is the part that matters here.
func doTenantSSO(t *testing.T, route http.HandlerFunc, host string, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/auth/sso?token="+ssoToken(t, ssoSecret, claims), nil)
	req.Host = host
	rec := httptest.NewRecorder()
	route(rec, req)
	return rec
}

// The load-bearing case. ONE email is handed to BOTH workspaces, which since D9 is
// legitimate — the unique on users is (workspace_id, email). Two users must exist, one per
// workspace, and each session must belong to its own.
//
// Unscoped, the second hand-off finds the FIRST workspace's user by email and mints a session
// for it: workspace B's visitor signs in as workspace A's person. The sessions row would even
// satisfy its foreign key, because sessions.user_id is global.
func TestSSOHandoff_multiTenantLandsInTheTokensWorkspace(t *testing.T) {
	platform, route := newSSOPairHandler(t)

	const shared = "shared@example.test"
	claimsA := ssoTenantClaims("acme", ssoHostA)
	claimsA["sub"] = shared
	claimsB := ssoTenantClaims("globex", ssoHostB)
	claimsB["sub"] = shared

	recA := doTenantSSO(t, route, ssoHostA, claimsA)
	if recA.Code != http.StatusFound {
		t.Fatalf("A: status = %d; want 302 — %s", recA.Code, recA.Body.String())
	}
	recB := doTenantSSO(t, route, ssoHostB, claimsB)
	if recB.Code != http.StatusFound {
		t.Fatalf("B: status = %d; want 302 — %s", recB.Code, recB.Body.String())
	}

	userA := userIDInWorkspace(t, platform, "acme", shared)
	userB := userIDInWorkspace(t, platform, "globex", shared)
	if userA == userB {
		t.Fatalf("both hand-offs resolved the same user %q; the second workspace was served "+
			"the first workspace's person", userA)
	}

	assertSession(t, platform, sessionCookieValue(t, recA), "acme", userA)
	assertSession(t, platform, sessionCookieValue(t, recB), "globex", userB)
}

// ⛔ A token for A presented on B's host: 403, and nothing created. This is the case
// host-based resolution exists for — with the workspace taken from `wid` alone, both the
// audience and the wid would check out and A's session would be created on B's domain.
func TestSSOHandoff_tokenForAnotherWorkspaceIsRefusedOnThisHost(t *testing.T) {
	platform, route := newSSOPairHandler(t)

	rec := doTenantSSO(t, route, ssoHostB, ssoTenantClaims("acme", ssoHostA))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 — %s", rec.Code, rec.Body.String())
	}
	if got := ssoErrorBody(t, rec); got != "workspace mismatch" {
		t.Errorf("error = %q; want \"workspace mismatch\" (D10's body)", got)
	}

	for _, table := range []string{"users", "sessions", "sso_nonces"} {
		var n int
		q := `SELECT COUNT(*) FROM ` + table
		if table != "sso_nonces" {
			q += ` WHERE workspace_id IN ('acme', 'globex')`
		}
		if err := platform.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows; a refused hand-off must create nothing — not even a "+
				"spent nonce, since the token was never usable here", table, n)
		}
	}
}

func TestSSOHandoff_multiTenantRefusesABadWID(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]any)
		status int
		want   string
	}{
		"missing": {func(c map[string]any) { delete(c, "wid") }, http.StatusUnauthorized, "wid is required"},
		"empty":   {func(c map[string]any) { c["wid"] = "" }, http.StatusUnauthorized, "wid is required"},
		// A wid that names another workspace, or nothing at all, is the same answer: it does
		// not match the host this token was spent at.
		"unknown":   {func(c map[string]any) { c["wid"] = "nosuchtenant" }, http.StatusForbidden, "workspace mismatch"},
		"not an id": {func(c map[string]any) { c["wid"] = "Not An Id!" }, http.StatusForbidden, "workspace mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			platform, route := newSSOPairHandler(t)
			claims := ssoTenantClaims("acme", ssoHostA)
			tc.mutate(claims)

			rec := doTenantSSO(t, route, ssoHostA, claims)
			if rec.Code != tc.status {
				t.Fatalf("status = %d; want %d — %s", rec.Code, tc.status, rec.Body.String())
			}
			if got := ssoErrorBody(t, rec); got != tc.want {
				t.Errorf("error = %q; want %q", got, tc.want)
			}
			var users int
			if err := platform.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
				t.Fatalf("count users: %v", err)
			}
			if users != 0 {
				t.Errorf("users = %d; a refused hand-off must create nobody", users)
			}
		})
	}
}

// The audience is the host the token is spent at, with no trailing slash. A token whose aud
// names the identity host is refused even on the right tenant host: the identity host cannot
// set the cookie this hand-off exists to set.
func TestSSOHandoff_multiTenantAudienceIsThePublicHost(t *testing.T) {
	cases := map[string]string{
		"the identity host": ssoBaseURL,
		"a bare hostname":   ssoHostA,
		"a trailing slash":  "https://" + ssoHostA + "/",
	}
	for name, aud := range cases {
		t.Run(name, func(t *testing.T) {
			_, route := newSSOPairHandler(t)
			claims := ssoTenantClaims("acme", ssoHostA)
			claims["aud"] = aud

			rec := doTenantSSO(t, route, ssoHostA, claims)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401 — %s", rec.Code, rec.Body.String())
			}
			if got := ssoErrorBody(t, rec); got != "aud does not match this instance" {
				t.Errorf("error = %q; want the audience to be named", got)
			}
		})
	}
}

// A replayed token loses on the nonce table's primary key, before a second session exists.
func TestSSOHandoff_multiTenantReplayIsRefused(t *testing.T) {
	platform, route := newSSOPairHandler(t)
	claims := ssoTenantClaims("acme", ssoHostA)

	if rec := doTenantSSO(t, route, ssoHostA, claims); rec.Code != http.StatusFound {
		t.Fatalf("first: %d — %s", rec.Code, rec.Body.String())
	}
	rec := doTenantSSO(t, route, ssoHostA, claims)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay: status = %d; want 401 — %s", rec.Code, rec.Body.String())
	}
	if got := ssoErrorBody(t, rec); got != "jti has already been used" {
		t.Errorf("error = %q; want the jti to be named", got)
	}

	var sessions int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE workspace_id = 'acme'`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Errorf("sessions = %d; want 1 — a replay must not mint a second", sessions)
	}
}

// An unrecognised host is a 404 before anything else happens: no tenant, nowhere to land.
func TestSSOHandoff_unknownHostIs404(t *testing.T) {
	_, route := newSSOPairHandler(t)
	rec := doTenantSSO(t, route, "nobody.example", ssoTenantClaims("acme", ssoHostA))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on a host that names no workspace", rec.Code)
	}
}

// A suspended workspace answers 503 with Retry-After on its own surfaces (D12), and a
// hand-off into one is the same answer: it lands in /admin/, which is suspended too.
func TestSSOHandoff_multiTenantRefusesASuspendedWorkspace(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL(ssoBaseURL)
	h.SetSSOSecret(ssoSecret)
	seedSSOWorkspace(t, platform, "acme", ssoHostA, "suspended")

	rec := doTenantSSO(t, h.Platform((*handler.Handler).SSOHandoff), ssoHostA,
		ssoTenantClaims("acme", ssoHostA))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 — %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header on the 503")
	}
}

// The mint half: the OAuth callback hands off to the workspace's public host instead of
// setting a cookie on the identity host, and the token it mints is one this endpoint accepts.
//
// The two halves are exercised together on purpose — a mint that produced a token the
// verifier rejects would pass any test that only looked at the redirect.
func TestOAuthHandoff_callbackMintsATokenTheSSOEndpointAccepts(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL(ssoBaseURL)
	h.SetSSOSecret(ssoSecret)
	seedSSOWorkspace(t, platform, "acme", ssoHostA, "active")
	seedSSOWorkspace(t, platform, "globex", ssoHostB, "active")

	// The person exists in acme. The same address in globex is what makes an unscoped lookup
	// wrong rather than merely untidy.
	if _, err := platform.Exec(`
		INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner)
		VALUES ('acme-user', 'acme', 'both@example.test', 'Acme Person', 'UTC', 1, 1),
		       ('globex-user', 'globex', 'both@example.test', 'Globex Person', 'UTC', 1, 1)`); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	target := handler.FinishOAuthLoginForTest(h, "both@example.test", "acme")
	if target == "" {
		t.Fatal("the callback set a cookie instead of handing off; multi-tenant must redirect")
	}
	if !strings.HasPrefix(target, "https://"+ssoHostA+"/v1/auth/sso?token=") {
		t.Fatalf("hand-off target = %q; want the workspace's public host and /v1/auth/sso", target)
	}

	// Spend the minted token at that host, through the real endpoint.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target[len("https://"+ssoHostA):], nil)
	req.Host = ssoHostA
	h.Platform((*handler.Handler).SSOHandoff)(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("spending the minted token: status = %d; want 302 — %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q; want /admin/", loc)
	}
	assertSession(t, platform, sessionCookieValue(t, rec), "acme", "acme-user")

	// ⛔ And globex's identically-addressed user got no session. Before D11 the callback's
	// lookup was `WHERE email = ?` on the platform handle, which resolves an arbitrary one of
	// these two rows.
	var globexSessions int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE workspace_id = 'globex'`).Scan(&globexSessions); err != nil {
		t.Fatalf("count globex sessions: %v", err)
	}
	if globexSessions != 0 {
		t.Errorf("globex has %d sessions; the login was for acme", globexSessions)
	}
}

// A minted token is bound to the workspace that started the login: spending acme's hand-off
// on globex's host is the same 403 as any other cross-workspace token.
func TestOAuthHandoff_mintedTokenIsRefusedOnAnotherWorkspacesHost(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL(ssoBaseURL)
	h.SetSSOSecret(ssoSecret)
	seedSSOWorkspace(t, platform, "acme", ssoHostA, "active")
	seedSSOWorkspace(t, platform, "globex", ssoHostB, "active")
	if _, err := platform.Exec(`
		INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner)
		VALUES ('acme-user', 'acme', 'person@example.test', 'Acme Person', 'UTC', 1, 1)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	target := handler.FinishOAuthLoginForTest(h, "person@example.test", "acme")
	if target == "" {
		t.Fatal("no hand-off target")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target[len("https://"+ssoHostA):], nil)
	req.Host = ssoHostB // the wrong tenant's host
	h.Platform((*handler.Handler).SSOHandoff)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 — %s", rec.Code, rec.Body.String())
	}
	var sessions int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions = %d; want 0", sessions)
	}
}

// Without CALNODE_SSO_SHARED_SECRET a multi-tenant OAuth login has nowhere to land, so it
// refuses rather than setting a cookie on the identity host — which would produce a session
// the person's own admin UI cannot see.
func TestOAuthHandoff_refusesWithoutTheSharedSecret(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL(ssoBaseURL)
	// no SetSSOSecret
	seedSSOWorkspace(t, platform, "acme", ssoHostA, "active")
	if _, err := platform.Exec(`
		INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner)
		VALUES ('acme-user', 'acme', 'person@example.test', 'Acme Person', 'UTC', 1, 1)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	target := handler.FinishOAuthLoginForTest(h, "person@example.test", "acme")
	if !strings.Contains(target, "error=sso") {
		t.Errorf("redirect = %q; want an error=sso refusal", target)
	}
	var sessions int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions = %d; want 0 — a login that cannot hand off must not half-succeed", sessions)
	}
}

func userIDInWorkspace(t *testing.T, platform *db.DB, workspaceID, email string) string {
	t.Helper()
	var id string
	if err := platform.QueryRow(
		`SELECT id FROM users WHERE workspace_id = ? AND email = ?`, workspaceID, email).Scan(&id); err != nil {
		t.Fatalf("no user %s in workspace %s: %v", email, workspaceID, err)
	}
	return id
}

func sessionCookieValue(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "calnode_session" {
			return c.Value
		}
	}
	t.Fatal("no calnode_session cookie was set")
	return ""
}

func assertSession(t *testing.T, platform *db.DB, sessionID, wantWorkspace, wantUser string) {
	t.Helper()
	var gotWorkspace, gotUser string
	if err := platform.QueryRow(
		`SELECT workspace_id, user_id FROM sessions WHERE id = ?`, sessionID).Scan(&gotWorkspace, &gotUser); err != nil {
		t.Fatalf("session %s: %v", sessionID, err)
	}
	if gotWorkspace != wantWorkspace {
		t.Errorf("session workspace = %q; want %q", gotWorkspace, wantWorkspace)
	}
	if gotUser != wantUser {
		t.Errorf("session user = %q; want %q", gotUser, wantUser)
	}
}
