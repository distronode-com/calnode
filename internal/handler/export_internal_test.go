package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"time"
)

// Test-only bridges. This file is a _test.go, so these identifiers exist for
// handler_test and are absent from any real build of the package — the alternative,
// exporting replaceReminderJobs, would widen the production API to serve one test.

// ReplaceReminderJobsForTest calls the unexported reminder replacement, so the external
// test package can drive the exact statement the reschedule path runs rather than a
// copy of it. A test that re-typed the SQL would pass with the production code broken.
func ReplaceReminderJobsForTest(h *Handler, ctx context.Context, bookingID, etID string, newStart time.Time) error {
	return h.replaceReminderJobs(ctx, bookingID, etID, newStart)
}

// FinishOAuthLoginForTest drives the OAuth callback's tail and returns the Location it
// redirected to, so the external test package can spend the token it minted against the real
// SSO endpoint. It returns "" if no redirect was issued.
//
// The tail is what D11 changed, and it is reachable no other way without standing up a fake
// Google: the exchange above it needs a provider. Driving it directly keeps the test about the
// hand-off rather than about oauth2.
func FinishOAuthLoginForTest(h *Handler, email, workspaceID string) string {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/callback", nil)
	// ⛔ Through Platform, because that is how the route is registered and it decides which
	// handle the tail runs on. Called on the bare handler the lookup would use the UNBOUND
	// application handle, which under the policies matches no row — the login would report
	// "no account" for a user that exists, which is precisely the failure mode this bridge
	// would otherwise hide.
	h.Platform(func(p *Handler, w http.ResponseWriter, r *http.Request) {
		p.finishOAuthLogin(w, r, email, workspaceID)
	})(rec, req)
	return rec.Header().Get("Location")
}
