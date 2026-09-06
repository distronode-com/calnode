package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/uid"
)

const (
	// ssoMaxTokenLifetime caps exp - iat. A hand-off is a redirect the browser follows
	// immediately, so it needs seconds, not minutes.
	ssoMaxTokenLifetime = 60 * time.Second

	// ssoClockSkew is how far the two systems' clocks may disagree before a token is
	// refused, applied to both ends of the window.
	ssoClockSkew = 30 * time.Second

	// ssoDefaultNext is where a hand-off lands when no ?next= is given.
	ssoDefaultNext = "/admin/"
)

// SetSSOSecret configures the shared secret for the SSO hand-off endpoint. An empty
// secret leaves the endpoint off (it 404s). Set once at boot from config, like
// SetBaseURL and SetEncKey, so there is no lock here.
func (h *Handler) SetSSOSecret(secret string) {
	h.ssoSecret = secret
}

// ssoClaims is the token payload. Every field except wid is required.
type ssoClaims struct {
	Iss  string `json:"iss"`  // issuing system, any non-empty string; logged, never authorised on
	Aud  string `json:"aud"`  // must equal this instance's BASE_URL
	Sub  string `json:"sub"`  // the person's email address
	Name string `json:"name"` // display name, used when the user is created
	Role string `json:"role"` // owner | admin | member
	Iat  int64  `json:"iat"`  // issued at, seconds since the epoch
	Exp  int64  `json:"exp"`  // expires at, seconds since the epoch
	JTI  string `json:"jti"`  // unique per token; replay is refused

	// WID names a workspace. Parsed and deliberately ignored: an instance is a single
	// workspace today, so there is nothing to select. A multi-workspace mode will use
	// it to decide which workspace the hand-off lands in, and accepting it now means a
	// caller written against that future does not have to be changed to work today.
	WID string `json:"wid"`
}

// SSOHandoff handles GET /v1/auth/sso?token=<jwt>[&next=/path] — the signed session
// hand-off. An external identity system that has already authenticated a person hands
// them into a Calnode session, without a second login.
//
// The token is a compact HS256 JWT signed with a secret the two systems share
// (CALNODE_SSO_SHARED_SECRET); the endpoint is off unless that is set. It is verified
// here rather than by a JWT library for the same reason internal/livekit signs its own:
// one algorithm, one key, a fixed claim set, and no dependency to keep current. Only
// HS256 is accepted — "none" and every asymmetric alg are refused before the signature
// is looked at, which is the classic JWT downgrade.
//
// Two properties do the security work, and neither is optional:
//
//   - The token is short-lived (exp at most 60s after iat) so a captured URL is not a
//     standing credential. 30s of clock skew is allowed in both directions, because the
//     two systems are separate hosts and NTP is not a guarantee.
//   - The token is single-use: its jti is claimed in sso_nonces before the session is
//     created, so a replay inside the validity window loses on the primary key.
//
// This is the ONE path that creates a user without an invite (see ssoResolveUser).
// Everywhere else an unknown email is refused; the shared secret is what makes this
// different — the caller is the operator's own identity system, not an arbitrary
// visitor with a Google account.
//
// Success is a 302 into the admin app (or ?next=, when that is a same-origin absolute
// path). Every failure is a 401 with a JSON body naming the claim that failed, so an
// operator wiring this up can tell a clock problem from a wrong audience — the body
// never carries the secret, the signature, or the token.
func (h *Handler) SSOHandoff(w http.ResponseWriter, r *http.Request) {
	if h.ssoSecret == "" {
		// Off unless CALNODE_SSO_SHARED_SECRET is set, and 404 rather than 501 so an
		// instance that has not configured SSO is indistinguishable from one that does
		// not implement it. Nothing is disclosed to a prober either way.
		h.writeError(w, http.StatusNotFound, "not found")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, "token is required")
		return
	}

	claims, err := verifySSOToken(token, h.ssoSecret)
	if err != nil {
		h.logger.WarnContext(r.Context(), "sso: token rejected", "reason", err.Error())
		h.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := claims.validate(h.baseURL, time.Now()); err != nil {
		h.logger.WarnContext(r.Context(), "sso: claims rejected", "reason", err.Error(), "iss", claims.Iss)
		h.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// Validated before the nonce is claimed: a bad ?next= is the caller's own bug, and
	// burning the token over it would make the retry fail for a second, unrelated reason.
	next, err := ssoNextPath(r.URL.Query().Get("next"))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Claim the jti before anything is created. On a replay this is the statement that
	// fails, and it fails on the primary key rather than on a read-then-write the second
	// request could interleave with.
	if _, err := h.db.ExecContext(r.Context(),
		`INSERT INTO sso_nonces (jti, expires_at) VALUES (?, ?)`,
		claims.JTI, time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339)); err != nil {
		if db.IsUniqueViolation(err) {
			h.logger.WarnContext(r.Context(), "sso: token replayed", "iss", claims.Iss)
			h.writeError(w, http.StatusUnauthorized, "jti has already been used")
			return
		}
		h.logger.ErrorContext(r.Context(), "sso: claim nonce", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	userID, created, err := h.ssoResolveUser(r.Context(), claims)
	switch {
	case errors.Is(err, errSSOArchived):
		h.writeError(w, http.StatusUnauthorized, "account is archived")
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "sso: resolve user", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.createSession(r.Context(), w, userID); err != nil {
		h.logger.ErrorContext(r.Context(), "sso: create session", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "sso: session handed off",
		"iss", claims.Iss, "user_id", userID, "user_created", created)
	http.Redirect(w, r, next, http.StatusFound)
}

// errSSOArchived reports that the token named a real but offboarded user. Archived
// means no login by every other path (see §6), and a shared secret does not change that.
var errSSOArchived = errors.New("sso: user is archived")

// ssoResolveUser maps the token's sub to a user id, creating the user when the email is
// unknown. It reports whether the user was created.
//
// Role handling is deliberately asymmetric. On creation the claim's role is applied, so
// the identity system provisions people directly. On an existing user the role is left
// alone — a workspace's roles are the workspace's business, and letting a hand-off
// rewrite them on every sign-in would make the admin UI's role controls advisory. The
// single exception is bootstrapping: a claim asking for owner is honoured when the
// instance has no owner, because the one-owner invariant TransferOwnership maintains
// means there is nothing to displace.
func (h *Handler) ssoResolveUser(ctx context.Context, claims ssoClaims) (userID string, created bool, err error) {
	email := strings.ToLower(strings.TrimSpace(claims.Sub))

	var archivedAt sql.NullString
	err = h.db.QueryRowContext(ctx,
		`SELECT id, archived_at FROM users WHERE email = ?`, email).Scan(&userID, &archivedAt)
	switch {
	case err == sql.ErrNoRows:
		// Unknown email: create. This is the one path that creates a user without an
		// invite; it is reachable only by a caller holding the shared secret.
		isAdmin, isOwner := 0, 0
		switch claims.Role {
		case "owner":
			isAdmin = 1
			if !h.ssoOwnerExists(ctx) {
				isOwner = 1
			}
		case "admin":
			isAdmin = 1
		}
		userID = uid.New()
		if _, err := h.db.ExecContext(ctx, `
			INSERT INTO users (id, email, name, iana_timezone, is_admin, is_owner, email_login)
			VALUES (?, ?, ?, 'UTC', ?, ?, 0)`,
			userID, email, claims.Name, isAdmin, isOwner); err != nil {
			return "", false, err
		}
		return userID, true, nil

	case err != nil:
		return "", false, err
	}

	if archivedAt.Valid {
		return "", false, errSSOArchived
	}
	if claims.Role == "owner" && !h.ssoOwnerExists(ctx) {
		if _, err := h.db.ExecContext(ctx,
			`UPDATE users SET is_owner = 1, is_admin = 1 WHERE id = ?`, userID); err != nil {
			return "", false, err
		}
	}
	return userID, false, nil
}

// ssoOwnerExists reports whether the instance already has an owner. A read error is
// reported as "yes" so a failed check never hands out ownership.
func (h *Handler) ssoOwnerExists(ctx context.Context) bool {
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE is_owner = 1 AND archived_at IS NULL`).Scan(&n); err != nil {
		h.logger.ErrorContext(ctx, "sso: count owners", "error", err)
		return true
	}
	return n > 0
}

// ssoNextPath validates the optional ?next= target, returning the default when it is
// absent. Only a same-origin absolute path is allowed; anything else is refused rather
// than sanitised, because a redirect built from a partially-cleaned value is how open
// redirects survive their own fix.
func ssoNextPath(next string) (string, error) {
	if next == "" {
		return ssoDefaultNext, nil
	}
	switch {
	case !strings.HasPrefix(next, "/"):
		return "", errors.New("next must be an absolute path")
	case strings.HasPrefix(next, "//"):
		// Protocol-relative: "//evil.example" is another origin, not a local path.
		return "", errors.New("next must not start with //")
	case strings.Contains(next, `\`):
		// Some browsers normalise a backslash to a slash, so "/\evil.example" is a
		// protocol-relative URL wearing a disguise.
		return "", errors.New(`next must not contain a backslash`)
	case strings.Contains(next, "://"):
		return "", errors.New("next must not contain a scheme")
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			// A CR or LF would split the Location header; the rest are never legitimate
			// in a path either.
			return "", errors.New("next must not contain control characters")
		}
	}
	return next, nil
}

// verifySSOToken parses a compact JWS and verifies its HS256 signature with secret,
// returning the claims. The error names what was wrong in terms the operator can act
// on, and never echoes any part of the token back — an error body is attacker-reachable.
func verifySSOToken(token, secret string) (ssoClaims, error) {
	var claims ssoClaims

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("token is not a three-part JWT")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, errors.New("token header is not base64url")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return claims, errors.New("token header is not JSON")
	}
	// Checked before the signature: accepting the token's own choice of algorithm is the
	// JWT downgrade attack ("none", or an RS256 key confusion), and the message stays
	// fixed rather than quoting the value back.
	if header.Alg != "HS256" {
		return claims, errors.New("token alg must be HS256")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, errors.New("token signature is not base64url")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) { // constant time
		return claims, errors.New("token signature does not verify")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, errors.New("token payload is not base64url")
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return claims, errors.New("token payload is not JSON")
	}
	return claims, nil
}

// validate checks the claim set against this instance and the current time. aud is the
// instance's BASE_URL: binding the token to one audience is what stops a token minted
// for a staging instance being spent on production, when both share a secret by mistake.
func (c ssoClaims) validate(aud string, now time.Time) error {
	if c.Iss == "" {
		return errors.New("iss is required")
	}
	if c.Aud == "" || c.Aud != aud {
		return errors.New("aud does not match this instance")
	}
	// A full RFC 5322 parse is not the point; the value is looked up against a column
	// whose contents are addresses, so this only rejects the obviously-not-an-address.
	if s := strings.TrimSpace(c.Sub); s == "" || !strings.Contains(s, "@") {
		return errors.New("sub must be an email address")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	switch c.Role {
	case "owner", "admin", "member":
	default:
		return errors.New("role must be owner, admin or member")
	}
	if c.JTI == "" {
		return errors.New("jti is required")
	}
	if c.Iat == 0 {
		return errors.New("iat is required")
	}
	if c.Exp == 0 {
		return errors.New("exp is required")
	}

	iat, exp := time.Unix(c.Iat, 0), time.Unix(c.Exp, 0)
	if !exp.After(iat) {
		return errors.New("exp must be after iat")
	}
	if exp.Sub(iat) > ssoMaxTokenLifetime {
		return errors.New("exp is more than 60s after iat")
	}
	if iat.After(now.Add(ssoClockSkew)) {
		return errors.New("iat is in the future")
	}
	if exp.Before(now.Add(-ssoClockSkew)) {
		return errors.New("exp is in the past")
	}
	return nil
}
