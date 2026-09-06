package handler

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/uid"
)

// newOAuthState generates a CSRF state token, sets it as a short-lived cookie, and
// returns the value to put in the provider's authorize URL. Shared by the Google
// and Microsoft sign-in flows.
//
// ⛔ The workspace rides the COOKIE, never the URL. The cookie value is
// `<nonce>|<workspace_id>` and only the nonce goes to the provider, so the value that
// decides which tenant a login lands in is one this server wrote and the browser only
// echoed back. A visitor can rewrite the `state` query parameter; doing so just fails the
// comparison. Putting the workspace there instead would let anyone choose the tenant their
// Google identity is admitted to.
//
// The workspace is known HERE and not at the callback: the person clicked "sign in with
// Google" on their own public host, and the callback arrives on the identity host, which
// names no tenant. That asymmetry is the whole reason this parameter exists (D11).
func (h *Handler) newOAuthState(w http.ResponseWriter, workspaceID string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := hex.EncodeToString(b)
	cookieValue := state
	if workspaceID != "" {
		cookieValue = state + oauthStateSep + workspaceID
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly/SameSite/Secure are all set; Secure is h.secureCookie (dynamic on BASE_URL scheme), which gosec's static check can't verify
		Name:     stateCookieName,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   int(stateDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie,
	})
	return state, nil
}

// oauthStateSep separates the CSRF nonce from the workspace id inside the state cookie.
// A workspace id is [a-z0-9_-]{1,64} and a nonce is hex, so neither can contain it.
const oauthStateSep = "|"

// verifyOAuthState checks the ?state param against the state cookie, clears the cookie
// regardless (single use), and returns the workspace the login was started from.
//
// The nonce is compared; the workspace is READ. That split is the security property: the
// query parameter only has to match, so an attacker rewriting it achieves a failed login,
// while the value that selects the tenant never left this server's cookie.
func (h *Handler) verifyOAuthState(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(stateCookieName)
	nonce, workspaceID := "", ""
	if err == nil {
		nonce = c.Value
		if i := strings.Index(c.Value, oauthStateSep); i >= 0 {
			nonce, workspaceID = c.Value[:i], c.Value[i+1:]
		}
	}
	ok := err == nil && nonce != "" && r.URL.Query().Get("state") == nonce
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly/SameSite/Secure are all set; Secure is h.secureCookie (dynamic on BASE_URL scheme), which gosec's static check can't verify
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie,
	})
	return workspaceID, ok
}

// finishOAuthLogin resolves an OAuth-verified email to an existing, non-archived user and
// starts a session. Self-registration is not allowed — only known emails sign in.
//
// workspaceID is the tenant the login STARTED from, recovered from the state cookie. It is
// "" in single-tenant mode, where this function behaves exactly as it always has: look the
// email up, create a session, redirect to /admin.
//
// ⛔ In multi-tenant mode two things change, and both were bugs before D11 landed:
//
//  1. The lookup is scoped. `SELECT id FROM users WHERE email = ?` on the platform handle
//     resolves an ARBITRARY workspace's user when the same address exists in several — which
//     since D9 is legitimate and ordinary — and then starts a session for that stranger.
//  2. The session is not set here. This callback runs on the IDENTITY host
//     (GET /v1/auth/callback, Platform), and a cookie for the identity host is no use to a
//     person whose admin UI is on their own domain. So it mints a short-lived SSO token for
//     the workspace and redirects to that workspace's public host, which is the only place
//     the cookie can be set (D11).
//
// The MCP Connect return is the deliberate exception: /oauth/authorize is an identity-host
// endpoint whose consent-step cookies were set there, so sending it to a tenant's public
// host would arrive with none of them. That tail keeps its identity-host cookie.
func (h *Handler) finishOAuthLogin(w http.ResponseWriter, r *http.Request, email, workspaceID string) {
	email = strings.ToLower(strings.TrimSpace(email))

	ws := DefaultWorkspace
	if h.multiTenant {
		if workspaceID == "" {
			// The state cookie carried no workspace, so the login did not start on a
			// tenant's host. Nothing can be resolved from an OAuth identity alone.
			h.logger.WarnContext(r.Context(), "auth: oauth login with no workspace in state")
			http.Redirect(w, r, "/admin/login?error=workspace", http.StatusFound)
			return
		}
		resolved, err := h.workspaceByID(r.Context(), workspaceID)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "auth: resolve workspace from state",
				"error", err, "workspace_id", workspaceID)
			http.Redirect(w, r, "/admin/login?error=workspace", http.StatusFound)
			return
		}
		if resolved.Suspended() {
			http.Redirect(w, r, "/admin/login?error=suspended", http.StatusFound)
			return
		}
		ws = resolved
	}

	var userID string
	var archivedAt sql.NullString
	// workspace_id is named even in single-tenant mode, where it is 'default' and every row
	// carries it: one statement, one behaviour, and no branch that could drift.
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT id, archived_at FROM users WHERE workspace_id = ? AND email = ?`,
		ws.ID, email).Scan(&userID, &archivedAt); err != nil {
		h.logger.WarnContext(r.Context(), "auth: no account for email",
			"email", email, "workspace_id", ws.ID)
		http.Redirect(w, r, "/admin/login?error=no_account", http.StatusFound)
		return
	}
	if archivedAt.Valid {
		http.Redirect(w, r, "/admin/login?error=archived", http.StatusFound)
		return
	}

	// If this login was initiated by an MCP "Connect" flow, return to /oauth/authorize (the
	// consent step) on THIS host, with the cookie set here — see the note above.
	if dest, ok := h.consumeOAuthReturn(w, r); ok {
		if err := h.createSessionIn(r.Context(), w, userID, sessionWorkspace(h, ws)); err != nil {
			h.logger.ErrorContext(r.Context(), "auth: create session", "error", err)
			http.Redirect(w, r, "/admin/login?error=session", http.StatusFound)
			return
		}
		http.Redirect(w, r, dest, http.StatusFound) // #nosec G710 -- dest is validated by safeLocalPath (only "/oauth/authorize", never "//") both when set and when consumed; gosec's taint analysis can't trace through that check
		return
	}

	if !h.multiTenant {
		if err := h.createSessionIn(r.Context(), w, userID, ""); err != nil {
			h.logger.ErrorContext(r.Context(), "auth: create session", "error", err)
			http.Redirect(w, r, "/admin/login?error=session", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}

	// Multi-tenant: hand off to the workspace's own host, which is where the cookie belongs.
	token, err := h.mintSSOToken(ws, email, ssoHandoffName(r), "member")
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: mint sso token", "error", err,
			"workspace_id", ws.ID)
		http.Redirect(w, r, "/admin/login?error=sso", http.StatusFound)
		return
	}
	target := "https://" + ws.PublicHost + "/v1/auth/sso?token=" + url.QueryEscape(token) +
		"&next=" + url.QueryEscape(ssoDefaultNext)
	h.logger.InfoContext(r.Context(), "auth: handing off to the workspace host",
		"workspace_id", ws.ID, "public_host", ws.PublicHost)
	http.Redirect(w, r, target, http.StatusFound) // #nosec G710 -- ws.PublicHost comes from the workspaces row, not from the request
}

// sessionWorkspace is the workspace id a session row should name: nothing in single-tenant
// mode (the column default is correct there and the statement stays as it was), the resolved
// workspace otherwise, because a Platform route's handle binds nothing.
func sessionWorkspace(h *Handler, ws *Workspace) string {
	if !h.multiTenant {
		return ""
	}
	return ws.ID
}

// ssoHandoffName is the display name to create a user with, if the hand-off ends up creating
// one. The OAuth callback knows the verified email but not always a name, and a hand-off with
// an empty name is refused by the SSO endpoint's own validation — so the local part stands in
// until the person edits it.
func ssoHandoffName(r *http.Request) string {
	if n := strings.TrimSpace(r.URL.Query().Get("name")); n != "" {
		return n
	}
	return "New user"
}

// mintSSOToken signs the hand-off token the SSO endpoint verifies (D11).
//
// ⛔ role is always "member". The OAuth callback is not an authority on roles: it knows that
// Google or Microsoft vouched for an email address, which says nothing about what that person
// may do here. ssoResolveUser leaves an existing user's role alone, so this value only ever
// applies to a user it creates.
//
// exp is 30s out, half the endpoint's 60s ceiling: the token is spent by a browser following
// a redirect it already has in hand.
func (h *Handler) mintSSOToken(ws *Workspace, email, name, role string) (string, error) {
	if h.ssoSecret == "" {
		// ⛔ Not a silent fallback. Without the shared secret the hand-off endpoint 404s, so
		// there is nowhere for this login to land, and setting a cookie on the identity host
		// instead would produce a session the person's admin UI cannot see.
		return "", fmt.Errorf("CALNODE_SSO_SHARED_SECRET is not set, so a multi-tenant OAuth login has nowhere to hand off to")
	}
	now := time.Now().UTC()
	claims := ssoClaims{
		Iss:  h.baseURL,
		Aud:  "https://" + ws.PublicHost,
		Sub:  email,
		Name: name,
		Role: role,
		Iat:  now.Unix(),
		Exp:  now.Add(30 * time.Second).Unix(),
		JTI:  uid.New(),
		WID:  ws.ID,
	}
	return signSSOToken(claims, h.ssoSecret)
}
