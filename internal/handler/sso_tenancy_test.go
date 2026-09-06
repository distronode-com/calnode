package handler_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The SSO hand-off in multi-tenant mode (D11, and rule 1 of the platform-hooks merge).
//
// /v1/auth/sso is a Platform route: no tenant Host to resolve from and no credential
// yet, because the TOKEN is the credential. So the workspace comes from the `wid` claim,
// and every row the endpoint writes has to NAME it — the platform handle bypasses the
// policies and binds '', so neither RLS nor the column default will do it for us.
//
// These need the real pair: a single handle cannot tell two workspaces apart, and a
// superuser handle would satisfy every assertion whether the scoping existed or not.

const (
	ssoHostA = "book.acme.example"
	ssoHostB = "book.globex.example"
)

// newSSOPairHandler returns a multi-tenant handler over a real OpenPair, plus the
// platform handle the assertions read through, plus the platform-wrapped route exactly
// as server.New registers it.
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

// ssoTenantClaims is a valid claim set for one workspace: wid names it and aud is its
// own public host, which is where the token is spent (the identity host cannot set a
// cookie for that domain, which is the entire reason this endpoint exists).
func ssoTenantClaims(wid, host string) map[string]any {
	c := ssoClaimSet()
	c["wid"] = wid
	c["aud"] = "https://" + host
	return c
}

func doTenantSSO(route http.HandlerFunc, claims map[string]any, t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	route(rec, httptest.NewRequest(http.MethodGet,
		"/v1/auth/sso?token="+ssoToken(t, ssoSecret, claims), nil))
	return rec
}

// The load-bearing case. ONE email is handed to BOTH workspaces, which since D9 is
// legitimate — the unique on users is (workspace_id, email). Two users must exist, one
// per workspace, and each session must belong to its own.
//
// Unscoped, the second hand-off finds the FIRST workspace's user by email and mints a
// session for it: workspace B's visitor signs in as workspace A's person. The sessions
// row would even satisfy its foreign key, because sessions.user_id is global.
func TestSSOHandoff_multiTenantLandsInTheTokensWorkspace(t *testing.T) {
	platform, route := newSSOPairHandler(t)

	const shared = "shared@example.test"
	claimsA := ssoTenantClaims("acme", ssoHostA)
	claimsA["sub"] = shared
	claimsB := ssoTenantClaims("globex", ssoHostB)
	claimsB["sub"] = shared

	recA := doTenantSSO(route, claimsA, t)
	if recA.Code != http.StatusFound {
		t.Fatalf("A: status = %d; want 302 — %s", recA.Code, recA.Body.String())
	}
	recB := doTenantSSO(route, claimsB, t)
	if recB.Code != http.StatusFound {
		t.Fatalf("B: status = %d; want 302 — %s", recB.Code, recB.Body.String())
	}

	userA := userIDInWorkspace(t, platform, "acme", shared)
	userB := userIDInWorkspace(t, platform, "globex", shared)
	if userA == userB {
		t.Fatalf("both hand-offs resolved the same user %q; the second workspace was served "+
			"the first workspace's person", userA)
	}

	// Each session names its own workspace and its own workspace's user. The
	// workspace_id matters beyond bookkeeping: every later request for that user runs
	// on a BOUND handle, which could neither read nor delete a session filed elsewhere.
	assertSession(t, platform, sessionCookieValue(t, recA), "acme", userA)
	assertSession(t, platform, sessionCookieValue(t, recB), "globex", userB)

	var users int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM users WHERE email = ?`, shared).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 2 {
		t.Errorf("users with that address = %d; want 2, one per workspace", users)
	}
}

func TestSSOHandoff_multiTenantRefusesABadWID(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"missing": {func(c map[string]any) { delete(c, "wid") }, "wid is required"},
		"empty":   {func(c map[string]any) { c["wid"] = "" }, "wid is required"},
		"unknown": {func(c map[string]any) { c["wid"] = "nosuchtenant" }, "wid does not name a workspace"},
		// Refused before it can reach the database as a bound handle that matches
		// nothing, which is the silent version of the same answer.
		"not an id": {func(c map[string]any) { c["wid"] = "Not An Id!" }, "wid does not name a workspace"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			platform, route := newSSOPairHandler(t)
			claims := ssoTenantClaims("acme", ssoHostA)
			tc.mutate(claims)

			rec := doTenantSSO(route, claims, t)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401 — %s", rec.Code, rec.Body.String())
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

// In multi-tenant mode the audience is the WORKSPACE's public host, not BASE_URL. A
// token minted for tenant A must not be spendable on tenant B, and the identity host is
// not an audience at all — it cannot set the cookie the hand-off exists to set.
func TestSSOHandoff_multiTenantAudienceIsThePublicHost(t *testing.T) {
	cases := map[string]string{
		"the identity host": ssoBaseURL,
		"another tenant":    "https://" + ssoHostB,
		"a bare hostname":   ssoHostA,
	}
	for name, aud := range cases {
		t.Run(name, func(t *testing.T) {
			_, route := newSSOPairHandler(t)
			claims := ssoTenantClaims("acme", ssoHostA)
			claims["aud"] = aud

			rec := doTenantSSO(route, claims, t)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401 — %s", rec.Code, rec.Body.String())
			}
			if got := ssoErrorBody(t, rec); got != "aud does not match this instance" {
				t.Errorf("error = %q; want the audience to be named", got)
			}
		})
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

	rec := doTenantSSO(h.Platform((*handler.Handler).SSOHandoff), ssoTenantClaims("acme", ssoHostA), t)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 — %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header on the 503")
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
