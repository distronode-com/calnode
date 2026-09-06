package handler

import (
	"context"
	"encoding/json"
	"strings"
)

// Vendor webhooks and the workspace they belong to (B6).
//
// LiveKit and Stripe call us, not a tenant. There is no tenant Host (the URL was registered
// once, in the vendor's dashboard) and no credential of ours (the signature is the vendor's),
// so both routes are Platform-classified and must resolve their workspace from the ROW the
// event names.
//
// ⛔ THE WORKSPACE COMES FROM OUR OWN ROW, NEVER FROM THE PAYLOAD. Every resolver here names
// workspace_id in its SELECT list and keys the read on an id we issued — an egress id, a room
// name, a checkout session id. A `workspace_id` field in a vendor payload would be a tenant
// selector supplied by whoever can forge a payload, and the whole point of these functions is
// that no such field is consulted.
//
// ⚠️ AND THE ORDER OF VERIFY-VS-RESOLVE DIFFERS BY MODE, WHICH IS NOT A STYLE CHOICE.
// Both vendors' credentials live in server_settings, i.e. PER WORKSPACE (D7). On a
// Platform-wrapped route the handle is the platform handle, which bypasses the policies — so
// LoadLiveKitSettingsFromDB there reads `WHERE id = 1` across every tenant and returns an
// ARBITRARY workspace's API secret. Verifying a signature against a randomly chosen tenant's
// key is not verification, and there is no process-wide credential to use instead, because
// boot priming is single-tenant-only (B5, deliverable 4).
//
// So in multi-tenant mode the resolve happens FIRST and the signature is then checked with
// that workspace's own credentials. Two properties make that safe, and both are load-bearing:
//
//   - Nothing is WRITTEN before the signature verifies. The resolve is one keyed SELECT.
//   - Nothing is DISCLOSED. The response body is empty either way.
//
// ⚠️ The residual is an existence oracle: an unsigned request naming a real egress id gets 403
// (bad signature) while one naming an unknown id gets 200. The ids are opaque and unguessable
// (`booking-<uuid>` rooms, LiveKit egress ids), and anyone holding a valid signature already
// knows the id they sent, so the trade is worth stating rather than closing — closing it would
// mean 403 for an unknown row, and a vendor retrying that forever is the failure this design
// exists to avoid.
//
// In single-tenant mode the old order is kept exactly: verify, then act. There is one workspace
// and one set of credentials, so there is nothing to resolve and no reason to change what a
// working deployment does.

// livekitEventWorkspace resolves the workspace of a LiveKit event from the recording or booking
// the event names. It reports false when no row owns it, which the caller answers 2xx-and-ignore.
//
// Three keys, in order of how specific they are:
//
//  1. the egress id, which is on a recordings row we wrote when the egress started;
//  2. the room name, for a room that has any recording row at all;
//  3. the booking the room name encodes (`booking-<id>`), for a room event on a meeting that
//     has never been recorded — `room_finished` arrives for those too.
func (h *Handler) livekitEventWorkspace(ctx context.Context, egressID, room string) (*Handler, bool) {
	if !h.multiTenant {
		return h, true
	}
	var wsID string
	if egressID != "" {
		if err := h.platformDB().QueryRowContext(ctx,
			`SELECT workspace_id FROM recordings WHERE egress_id = ?`, egressID).Scan(&wsID); err == nil && wsID != "" {
			return h.workspaceForJob(wsID), true
		}
	}
	if room != "" {
		if err := h.platformDB().QueryRowContext(ctx,
			`SELECT workspace_id FROM recordings WHERE room = ? ORDER BY created_at DESC LIMIT 1`, room).
			Scan(&wsID); err == nil && wsID != "" {
			return h.workspaceForJob(wsID), true
		}
		// A room with no recording. Booking rooms are named booking-<booking id>, which is
		// the only room name this application creates, so the booking resolves the tenant.
		if id := strings.TrimPrefix(room, "booking-"); id != room && id != "" {
			if err := h.platformDB().QueryRowContext(ctx,
				`SELECT workspace_id FROM bookings WHERE id = ?`, id).Scan(&wsID); err == nil && wsID != "" {
				return h.workspaceForJob(wsID), true
			}
		}
	}
	return nil, false
}

// stripeEventWorkspace resolves the workspace of a Stripe event from the booking the checkout
// session belongs to. It reports false when no row owns it.
//
// The session id is preferred over the payload's metadata: `bookings.stripe_session_id` is a
// column we wrote when we created the session, whereas metadata is a field that travels in the
// event. The metadata booking id is the fallback for a session created before that column was
// populated, and even then the workspace comes from the ROW rather than the payload.
func (h *Handler) stripeEventWorkspace(ctx context.Context, sessionID, metadataBookingID string) (*Handler, bool) {
	if !h.multiTenant {
		return h, true
	}
	var wsID string
	if sessionID != "" {
		if err := h.platformDB().QueryRowContext(ctx,
			`SELECT workspace_id FROM bookings WHERE stripe_session_id = ?`, sessionID).Scan(&wsID); err == nil && wsID != "" {
			return h.workspaceForJob(wsID), true
		}
	}
	if metadataBookingID != "" {
		if err := h.platformDB().QueryRowContext(ctx,
			`SELECT workspace_id FROM bookings WHERE id = ?`, metadataBookingID).Scan(&wsID); err == nil && wsID != "" {
			return h.workspaceForJob(wsID), true
		}
	}
	return nil, false
}

// peekVendorIDs pulls the few identifiers a resolver needs out of an as-yet-unverified body.
//
// ⚠️ It parses before the signature is checked, which is deliberate and bounded: the body is
// already length-limited by the caller, the destination is a fixed struct, and the values are
// used for nothing but a keyed lookup. Nothing here is trusted — the workspace comes from the
// row those ids find, and if they find none the event is ignored.
// peekStripeIDs reads the checkout session id and the metadata booking id out of an
// as-yet-unverified Stripe event body.
//
// The event's shape is {"data":{"object":{"id":"cs_...","metadata":{"booking_id":"..."}}}}. Only
// those two strings are taken, and only to key the lookup that finds the workspace; the
// authoritative parse is the one VerifyWebhook does after the tenant is known.
func peekStripeIDs(body []byte) (sessionID, metadataBookingID string) {
	var peek struct {
		Data struct {
			Object struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return "", ""
	}
	return peek.Data.Object.ID, peek.Data.Object.Metadata["booking_id"]
}
