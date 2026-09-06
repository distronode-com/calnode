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

// LiveKitEventWorkspaceForTest and StripeEventWorkspaceForTest expose the vendor-webhook
// resolvers, which is where the tenancy decision is actually made.
//
// ⛔ They are bridged rather than tested through the HTTP handler because verification needs a
// real vendor secret: an end-to-end test can only ever observe the 403, which says nothing
// about WHICH workspace the event resolved to. Both run through Platform, because that is the
// handle the route gives them — on the bare handler the reads would use the unbound application
// handle and resolve nothing.
func LiveKitEventWorkspaceForTest(h *Handler, egressID, room string) (string, bool) {
	var id string
	var found bool
	h.Platform(func(p *Handler, _ http.ResponseWriter, r *http.Request) {
		scoped, ok := p.livekitEventWorkspace(r.Context(), egressID, room)
		found = ok
		if ok {
			id = scoped.Workspace().ID
		}
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/livekit/webhook", nil))
	return id, found
}

func StripeEventWorkspaceForTest(h *Handler, sessionID, metadataBookingID string) (string, bool) {
	var id string
	var found bool
	h.Platform(func(p *Handler, _ http.ResponseWriter, r *http.Request) {
		scoped, ok := p.stripeEventWorkspace(r.Context(), sessionID, metadataBookingID)
		found = ok
		if ok {
			id = scoped.Workspace().ID
		}
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/stripe/webhook", nil))
	return id, found
}

// STTBaseURLForWorkspaceForTest answers what the notetaker would use for one
// workspace: the scoped handler's resolution, per-tenant column first.
func STTBaseURLForWorkspaceForTest(h *Handler, workspaceID string) string {
	return h.forWorkspace(&Workspace{ID: workspaceID}).sttBaseURL()
}
