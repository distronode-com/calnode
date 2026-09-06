package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// The sweep's two (c) findings, each with the test that would have caught it.
//
// Both are the same shape: a Platform-wrapped route writing a TENANT row. The
// platform handle binds '', and workspace_id defaults to
// COALESCE(current_setting('app.workspace_id', true), 'default') — so an omitted
// column does not fail, it lands the row in the default workspace. Nothing errors.
// The row is simply in a tenant nobody owns and no host reaches.

// TestSweep_oauthGrantLandsInTheOwnersWorkspace drives the real OAuth 2.1 flow
// that the MCP "Connect" button uses — register a client, consent, exchange the
// code — and asserts the resulting grant belongs to the consenting user's
// workspace rather than to 'default'.
//
// ⛔ Before the fix the token still WORKED: VerifyMCPBearer reads on the platform
// handle, which bypasses the policies, so an agent could connect and call tools.
// What broke was ownership — the workspace's Connected-apps page could neither
// list nor revoke the grant, and deleting the workspace would have left it behind.
// That is why the assertion is on the row's workspace_id and on the page, not on
// whether the token authenticates.
func TestSweep_oauthGrantLandsInTheOwnersWorkspace(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()

	// A browser session for A's owner, so /oauth/authorize finds a signed-in user
	// instead of bouncing through the login flow. sessions.id is globally unique
	// (D9) precisely so a cookie resolves before a tenant is known.
	const sessionID = "sess-acme-oauth"
	if _, err := f.app.ForWorkspace(f.a.id).ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
		sessionID, f.a.userID, "2099-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// 1. Dynamic client registration (RFC 7591). oauth_clients is a global table:
	//    a connector registration is per client APPLICATION, not per tenant.
	reg := f.do(t, http.MethodPost, "app.calnode.example", "/oauth/register", "",
		`{"client_name":"sweep-test","redirect_uris":["http://127.0.0.1:9999/cb"]}`)
	if reg.Code != http.StatusCreated && reg.Code != http.StatusOK {
		t.Fatalf("register client: status = %d: %s", reg.Code, reg.Body.String())
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(reg.Body.Bytes(), &client); err != nil {
		t.Fatalf("decode registration %q: %v", reg.Body.String(), err)
	}
	if client.ClientID == "" {
		t.Fatalf("no client_id in %s", reg.Body.String())
	}

	// 2. PKCE S256.
	const verifier = "sweep-test-verifier-0123456789abcdefghijklmnop"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// 3. Consent. The decision POST is what writes oauth_auth_codes.
	form := url.Values{
		"client_id":             {client.ClientID},
		"redirect_uri":          {"http://127.0.0.1:9999/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"state":                 {"xyz"},
		"decision":              {"allow"},
	}
	code := f.oauthConsent(t, sessionID, form)

	// 4. Token exchange. This is what writes oauth_access_tokens.
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:9999/cb"},
		"client_id":     {client.ClientID},
		"code_verifier": {verifier},
	}
	tok := f.postForm(t, "app.calnode.example", "/oauth/token", tokenForm)
	if tok.Code != http.StatusOK {
		t.Fatalf("token exchange: status = %d: %s", tok.Code, tok.Body.String())
	}

	// The assertion. Read through the PLATFORM handle, which sees every workspace,
	// so this is about where the row IS rather than what A can see.
	var codeWS, tokenWS string
	if err := f.plat.QueryRowContext(ctx,
		`SELECT workspace_id FROM oauth_auth_codes WHERE user_id = ?`, f.a.userID).Scan(&codeWS); err != nil {
		// The code is deleted on exchange (single use), so absence is expected;
		// only a row in the wrong workspace is a failure.
		codeWS = f.a.id
	}
	if codeWS != f.a.id {
		t.Errorf("the authorization code landed in workspace %q; want %q", codeWS, f.a.id)
	}
	if err := f.plat.QueryRowContext(ctx,
		`SELECT workspace_id FROM oauth_access_tokens WHERE user_id = ?`, f.a.userID).Scan(&tokenWS); err != nil {
		t.Fatalf("read the issued token: %v", err)
	}
	if tokenWS != f.a.id {
		t.Errorf("the access token landed in workspace %q; want %q", tokenWS, f.a.id)
	}
	if tokenWS == db.DefaultWorkspaceID {
		t.Error("the grant landed in the default workspace — a Platform route wrote a tenant row without naming workspace_id")
	}

	// The observable consequence, which is what a user would report: the grant has
	// to be listed on the workspace's Connected-apps page, which is a
	// credential-scoped route running on the BOUND handle.
	list := f.do(t, http.MethodGet, f.a.host, "/v1/oauth/connections", f.a.apiKey, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list connections: status = %d: %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "sweep-test") {
		t.Errorf("A's Connected-apps page does not list the grant it just authorised: %s", list.Body.String())
	}
	// And B cannot see it.
	other := f.do(t, http.MethodGet, f.b.host, "/v1/oauth/connections", f.b.apiKey, "")
	if strings.Contains(other.Body.String(), "sweep-test") {
		t.Errorf("B's Connected-apps page lists A's grant: %s", other.Body.String())
	}
}

// TestSweep_setupIsRefusedInMultiTenantMode. POST /v1/setup is Platform-wrapped
// and creates the first user plus a live API key. Unrefused, both land in the
// default workspace: a working credential in a tenant nobody owns and no host
// reaches. Workspaces and their owners are provisioned through the platform API.
func TestSweep_setupIsRefusedInMultiTenantMode(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()

	before := f.countIn(t, "users", db.DefaultWorkspaceID)

	rec := f.do(t, http.MethodPost, "app.calnode.example", "/v1/setup", "",
		`{"name":"Squatter","email":"squatter@example.com","timezone":"UTC"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /v1/setup: status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	if after := f.countIn(t, "users", db.DefaultWorkspaceID); after != before {
		t.Errorf("users in the default workspace went from %d to %d — setup wrote a row", before, after)
	}
	var keys int
	if err := f.plat.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE workspace_id = ?`, db.DefaultWorkspaceID).Scan(&keys); err != nil {
		t.Fatalf("count default-workspace api keys: %v", err)
	}
	if keys != 0 {
		t.Errorf("%d API keys exist in the default workspace", keys)
	}
}

// TestSweep_tenantLocalTokensDoNotResolveAcrossHosts is the (b) half of the
// table: magic links, invites and manage tokens are read on the BOUND handle,
// because the route that reads them is host-scoped and the email that carried
// them was sent from that workspace's own public host. The test is that a token
// of B presented on A's host resolves to nothing.
func TestSweep_tenantLocalTokensDoNotResolveAcrossHosts(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()

	hB := f.app.ForWorkspace(f.b.id)

	// A magic-link token for B's user. token_hash is globally unique, so nothing
	// but the binding stops A's host from finding it.
	const magic = "magic-for-globex"
	sum := sha256.Sum256([]byte(magic))
	hash := encodeHex(sum[:])
	if _, err := hB.ExecContext(ctx,
		`INSERT INTO magic_link_tokens (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hash, f.b.userID, "2099-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed magic link: %v", err)
	}

	// On B's own host it is a valid login.
	own := f.do(t, http.MethodGet, f.b.host, "/v1/auth/magic-link/verify?token="+magic, "", "")
	if own.Code >= 400 {
		t.Fatalf("B's magic link on B's host: status = %d: %s", own.Code, own.Body.String())
	}

	// A second token, because the first is single-use.
	const magic2 = "magic-for-globex-2"
	sum2 := sha256.Sum256([]byte(magic2))
	if _, err := hB.ExecContext(ctx,
		`INSERT INTO magic_link_tokens (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		encodeHex(sum2[:]), f.b.userID, "2099-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed second magic link: %v", err)
	}
	// On A's host it resolves to nothing, and must not mint a session.
	other := f.do(t, http.MethodGet, f.a.host, "/v1/auth/magic-link/verify?token="+magic2, "", "")
	for _, c := range other.Result().Cookies() {
		if c.Name == "calnode_session" && c.Value != "" {
			t.Fatalf("B's magic link minted a session on A's host")
		}
	}

	// A manage token for B's booking. The manage page is host-scoped, and D10 asks
	// that the token's booking belong to the request's workspace — which the policy
	// enforces without the handler carrying a predicate.
	const manage = "manage-for-globex"
	msum := sha256.Sum256([]byte(manage))
	if _, err := hB.ExecContext(ctx,
		`INSERT INTO booking_manage_tokens (token_hash, booking_id, expires_at) VALUES (?, ?, ?)`,
		encodeHex(msum[:]), f.b.bookingID, "2099-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed manage token: %v", err)
	}
	// ⚠️ The manage page answers 200 for an unknown token BY DESIGN — it is a
	// booker-facing page that renders "this link is no longer valid", not an API.
	// So the assertion is on the content: none of B's booking must appear.
	onA := f.do(t, http.MethodGet, f.a.host, "/manage/"+manage, "", "")
	for _, leak := range []string{f.b.bookingID, "GLOBEX", "booker-globex@example.com"} {
		if strings.Contains(onA.Body.String(), leak) {
			t.Errorf("B's manage token leaked %q on A's host", leak)
		}
	}
	onB := f.do(t, http.MethodGet, f.b.host, "/manage/"+manage, "", "")
	if onB.Code != http.StatusOK {
		t.Errorf("B's manage token on B's own host: status = %d", onB.Code)
	}
}

// oauthConsent posts the consent decision with the session cookie and the CSRF
// token the consent page hands out, and returns the authorization code.
func (f *tenancyFixture) oauthConsent(t *testing.T, sessionID string, form url.Values) string {
	t.Helper()

	// GET first: the consent page sets the CSRF cookie the POST has to echo.
	get := "/oauth/authorize?" + url.Values{
		"client_id":             form["client_id"],
		"redirect_uri":          form["redirect_uri"],
		"code_challenge":        form["code_challenge"],
		"code_challenge_method": form["code_challenge_method"],
		"response_type":         form["response_type"],
		"state":                 form["state"],
	}.Encode()

	r := newRequest(t, http.MethodGet, "app.calnode.example", get, nil)
	r.AddCookie(&http.Cookie{Name: "calnode_session", Value: sessionID})
	page := f.serve(r)
	if page.Code != http.StatusOK {
		t.Fatalf("consent page: status = %d: %s", page.Code, firstLine(page.Body.String()))
	}
	var csrf string
	for _, c := range page.Result().Cookies() {
		if strings.Contains(c.Name, "csrf") {
			csrf = c.Value
		}
	}
	if csrf != "" {
		form.Set("csrf", csrf)
		form.Set("csrf_token", csrf)
	}

	post := newRequest(t, http.MethodPost, "app.calnode.example", "/oauth/authorize",
		strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(&http.Cookie{Name: "calnode_session", Value: sessionID})
	if csrf != "" {
		post.AddCookie(&http.Cookie{Name: "calnode_oauth_csrf", Value: csrf})
	}
	res := f.serve(post)
	if res.Code != http.StatusFound && res.Code != http.StatusSeeOther {
		t.Fatalf("consent decision: status = %d: %s", res.Code, firstLine(res.Body.String()))
	}
	loc, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect %q: %v", res.Header().Get("Location"), err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in the redirect %q", res.Header().Get("Location"))
	}
	return code
}

func (f *tenancyFixture) countIn(t *testing.T, table, workspace string) int {
	t.Helper()
	var n int
	// The table name is a literal from this file, never input.
	if err := f.plat.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ?`, workspace).Scan(&n); err != nil {
		t.Fatalf("count %s in %s: %v", table, workspace, err)
	}
	return n
}

func encodeHex(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexDigits[c>>4]
		out[i*2+1] = hexDigits[c&0x0f]
	}
	return string(out)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
