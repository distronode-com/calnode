package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/calnode/calnode/internal/metrics"
)

// SetMetricsToken configures the bearer token that authorises GET /metrics. Empty leaves
// the endpoint off. Set once at boot from config, like SetSSOSecret.
func (h *Handler) SetMetricsToken(token string) {
	h.metricsToken = token
}

// Metrics handles GET /metrics — Prometheus text exposition of the counters in
// internal/metrics plus the job-queue depth read from the database.
//
// ⛔ Gated on `Authorization: Bearer <METRICS_TOKEN>`, and it answers **404** — byte-identical
// to the mux's own not-found — when the token is unset or wrong. Not 401: a 401 confirms the
// endpoint exists and invites a guess, and the numbers here are a business feed (bookings
// created per hour, request volume by surface) on an instance whose whole point is being
// publicly reachable. An operator who has not configured a token has not opted in to
// publishing any of that, so there is nothing to advertise.
//
// The response is not rate-limited: a scrape runs every few seconds by design, and a
// limiter tuned for humans would drop samples and produce gaps that look like downtime.
// The token is the control.
func (h *Handler) Metrics(w http.ResponseWriter, r *http.Request) {
	if !h.metricsAuthorized(r) {
		http.NotFound(w, r)
		return
	}

	// Job depth lives in the database because any instance can claim any job, so it is
	// read per scrape rather than counted in this process. One grouped query; a failure is
	// logged and reported as zero rather than failing the whole scrape, since the process
	// counters are still worth having when the database is the thing that is unwell (and
	// /readyz is the endpoint that answers "is the database reachable").
	var q metrics.Queue
	// ⛔ Platform(), not h.db. jobs is a tenant table (00060): the queue depth of an
	// INSTANCE is an instance-level number, so a bound read would report one
	// workspace's backlog as if it were the whole queue and the unbound handle would
	// report zero. Both are wrong in a way a dashboard cannot show you. /metrics is
	// registered through h.Platform, so h.db is already the platform handle — this is
	// belt and braces against someone re-registering the route Scoped.
	rows, err := h.db.Platform().QueryContext(r.Context(),
		`SELECT status, COUNT(*) FROM jobs WHERE status IN ('pending', 'failed') GROUP BY status`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "metrics: count jobs", "error", err)
	} else {
		for rows.Next() {
			var status string
			var n int64
			if err := rows.Scan(&status, &n); err != nil {
				h.logger.ErrorContext(r.Context(), "metrics: scan job count", "error", err)
				continue
			}
			switch status {
			case "pending":
				q.Pending = n
			case "failed":
				q.Failed = n
			}
		}
		if err := rows.Err(); err != nil {
			h.logger.ErrorContext(r.Context(), "metrics: job count rows", "error", err)
		}
		rows.Close() // #nosec G104 -- rows already fully consumed; nothing actionable on close error
	}

	w.Header().Set("Content-Type", metrics.ContentType)
	// Cache-Control matters here: a scrape must never be answered from an intermediary,
	// or a dashboard shows a frozen instance as a healthy one.
	w.Header().Set("Cache-Control", "no-store")
	if err := metrics.Write(w, q); err != nil {
		h.logger.ErrorContext(r.Context(), "metrics: write exposition", "error", err)
	}
}

// metricsAuthorized reports whether the request carries the configured bearer token.
//
// The comparison is over SHA-256 digests rather than the raw strings: subtle.ConstantTimeCompare
// returns early when the lengths differ, so comparing the values directly would leak the
// token's length. Hashing makes both sides 32 bytes whatever was sent.
func (h *Handler) metricsAuthorized(r *http.Request) bool {
	if h.metricsToken == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	presented := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
	expected := sha256.Sum256([]byte(h.metricsToken))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}
